package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	verificationLimit        = 5 * time.Minute
	verificationPoll         = time.Second
	maximumLogBytes          = 64 * 1024
	workloadNetwork          = "sunstone"
	maximumDNSLabel          = 63
	containerNameRandomBytes = 4
)

// errVerificationBudget reports that the health gate exhausted its own
// waiting budget between successful inspections of a container that kept
// running without becoming healthy: a known answer, so it is a definitive
// rejection rather than a lost transport or an inherited cancellation.
// An inspection still in flight when the budget expired stays an
// unanswered read and keeps lost-transport semantics.
var errVerificationBudget = errors.New("container did not become healthy within the verification budget")

// DockerRuntime performs container and routing operations on one connected VM
// through a Docker API socket and the Sunbeam control endpoint, both reached
// over the borrowed session transport.
type DockerRuntime struct {
	docker           *client.Client
	controlTransport *http.Transport
	secretTransport  *http.Transport
	secretClient     *http.Client
	secretsMu        sync.Mutex
	secretValues     map[string]string
	enrolledSecrets  []string
	routes           sunbeampbconnect.SunbeamClient
}

// NewDockerRuntime builds the Docker-backed runtime over an established
// session: Docker, the Sunbeam control endpoint at controlAddress, and
// secret fetches all use channels over the session's connection. The
// session is borrowed transport; a construction failure leaves it open for
// its owner, and closing the runtime releases the runtime's HTTP and Docker
// clients and its operation-scoped secret state — never the session.
func NewDockerRuntime(sess Session, controlAddress string) (*DockerRuntime, error) {
	if sess == nil {
		return nil, errors.New("session is required")
	}
	if controlAddress == "" {
		return nil, errors.New("Sunbeam control address is required")
	}

	dockerClient, err := client.New(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sess.DialContext(ctx, "unix", "/var/run/docker.sock")
		}),
		client.WithTimeout(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}

	controlTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sess.DialContext(ctx, "tcp", controlAddress)
		},
	}
	controlClient := &http.Client{Transport: controlTransport}
	secretTransport := &http.Transport{DialContext: sess.DialContext}
	secretClient := &http.Client{
		Transport:     secretTransport,
		Timeout:       secretPreparationLimit,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &DockerRuntime{
		docker:           dockerClient,
		controlTransport: controlTransport,
		secretTransport:  secretTransport,
		secretClient:     secretClient,
		routes:           sunbeampbconnect.NewSunbeamClient(controlClient, "http://sunbeam"),
	}, nil
}

// Close releases the runtime's operation-scoped resources: its HTTP and
// Docker clients and every fetched or enrolled secret value. The borrowed
// session stays open for its owner and remains usable by a fresh runtime.
func (h *DockerRuntime) Close() error {
	h.controlTransport.CloseIdleConnections()
	h.secretTransport.CloseIdleConnections()
	h.secretsMu.Lock()
	clear(h.secretValues)
	h.enrolledSecrets = nil
	h.secretsMu.Unlock()

	return h.docker.Close()
}

func (h *DockerRuntime) Pull(ctx context.Context, reference string) (Image, error) {
	pull, err := h.docker.ImagePull(ctx, reference, client.ImagePullOptions{})
	if err != nil {
		return Image{}, err
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err != nil {
		return Image{}, err
	}
	inspected, err := h.docker.ImageInspect(ctx, reference)
	if err != nil {
		return Image{}, err
	}
	image := Image{ID: inspected.ID}
	if inspected.Config != nil {
		image.DefaultCommand = slices.Clone(inspected.Config.Cmd)
		image.DefaultEnvironment = slices.Clone(inspected.Config.Env)
	}

	return image, nil
}

func (h *DockerRuntime) WorkloadContainers(ctx context.Context, workload string) ([]Container, error) {
	filters := make(client.Filters).
		Add("label", ManagedLabel+"=true").
		Add("label", WorkloadLabel+"="+workload)
	listed, err := h.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	containers := make([]Container, 0, len(listed.Items))
	for _, item := range listed.Items {
		name := ""
		if len(item.Names) != 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}

		healthy := item.State == container.StateRunning
		if item.Health != nil {
			healthy = string(item.Health.Status) == "healthy"
		}

		var metadata *DeployedMetadata
		if decoded, ok := DecodeMetadata(item.Labels); ok {
			metadata = &decoded
		}

		containers = append(containers, Container{
			ID:       item.ID,
			Name:     name,
			Image:    item.ImageID,
			Running:  item.State == container.StateRunning,
			Healthy:  healthy,
			Metadata: metadata,
		})
	}

	return containers, nil
}

func (h *DockerRuntime) Matches(ctx context.Context, current Container, desired ContainerSpec) (bool, error) {
	resolved, err := h.environment(ctx, desired)
	if err != nil {
		return false, err
	}
	inspected, err := h.docker.ContainerInspect(ctx, current.ID, client.ContainerInspectOptions{})
	if err != nil {
		return false, h.safeError(err)
	}
	if inspected.Container.Config == nil || inspected.Container.HostConfig == nil {
		return false, errors.New("container has incomplete configuration")
	}
	expectedCommand := desired.Command
	if expectedCommand == nil {
		expectedCommand = desired.Image.DefaultCommand
	}

	expectedEnvironment := environmentMap(desired.Image.DefaultEnvironment)
	maps.Copy(expectedEnvironment, resolved)
	actualEnvironment := environmentMap(inspected.Container.Config.Env)

	if desired.HTTP != nil && (inspected.Container.NetworkSettings == nil || inspected.Container.NetworkSettings.Networks[workloadNetwork] == nil) {
		return false, nil
	}

	return inspected.Container.Image == desired.Image.ID &&
		slices.Equal(inspected.Container.Config.Cmd, expectedCommand) &&
		maps.Equal(actualEnvironment, expectedEnvironment) &&
		string(inspected.Container.HostConfig.RestartPolicy.Name) == desired.RestartPolicy &&
		metadataMatches(inspected.Container.Config.Labels, desired), nil
}

func (h *DockerRuntime) Create(ctx context.Context, spec ContainerSpec) (Container, error) {
	suffix := make([]byte, containerNameRandomBytes)
	if _, err := rand.Read(suffix); err != nil {
		return Container{}, fmt.Errorf("generate container name: %w", err)
	}
	name := managedContainerName(spec.Name, hex.EncodeToString(suffix))
	// Labels carry references and literal configuration, never resolved payloads.
	environment, err := h.environment(ctx, spec)
	if err != nil {
		return Container{}, err
	}
	spec.Environment = environment
	created, err := h.docker.ContainerCreate(ctx, containerCreateOptions(spec, name))
	if err != nil {
		return Container{}, h.mutationError("create container", err)
	}

	return Container{ID: created.ID, Name: name}, nil
}

func (h *DockerRuntime) Start(ctx context.Context, target Container) error {
	_, err := h.docker.ContainerStart(ctx, target.ID, client.ContainerStartOptions{})

	return h.mutationError("start container", err)
}

func (h *DockerRuntime) BackendAddress(_ context.Context, target Container, containerPort uint16) (string, error) {
	if target.Name == "" {
		return "", errors.New("container has no network name")
	}

	return net.JoinHostPort(target.Name, strconv.FormatUint(uint64(containerPort), 10)), nil
}

func (h *DockerRuntime) Route(ctx context.Context, workload string) (Route, bool, error) {
	response, err := h.routes.GetRoute(ctx, connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/" + workload}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, err
	}

	return routeFromProto(response.Msg), true, nil
}

func (h *DockerRuntime) UpdateRoute(ctx context.Context, route Route) error {
	_, err := h.routes.UpdateRoute(ctx, connect.NewRequest(&sunbeampb.UpdateRouteRequest{
		Route: &sunbeampb.Route{
			Name: "routes/" + route.Workload,
			Backend: &sunbeampb.Backend{
				Address:          route.Address,
				ContainerId:      route.ContainerID,
				StartupProbePath: route.StartupProbePath,
			},
		},
		AllowMissing: true,
	}))

	return routeError("update route", err)
}

// routeError classifies route-control failures conservatively: only a
// genuine wire answer with a known rejection code proves Sunbeam did not
// apply the change — including a wire startup rejection, which keeps safe
// cleanup available. Transport-level codes, local deadline failures, and
// arbitrary server failures say nothing about the route's final state.
func routeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if connect.IsWireError(err) {
		switch connect.CodeOf(err) {
		case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeAlreadyExists:
			return err
		}
	}

	return unconfirmed(operation, err)
}

func (h *DockerRuntime) DeleteRoute(ctx context.Context, workload string, allowMissing bool) error {
	_, err := h.routes.DeleteRoute(ctx, connect.NewRequest(&sunbeampb.DeleteRouteRequest{
		Name:         "routes/" + workload,
		AllowMissing: allowMissing,
	}))

	return routeError("delete route", err)
}

func routeFromProto(route *sunbeampb.Route) Route {
	return Route{
		Workload:         strings.TrimPrefix(route.GetName(), "routes/"),
		Address:          route.GetBackend().GetAddress(),
		ContainerID:      route.GetBackend().GetContainerId(),
		StartupProbePath: route.GetBackend().GetStartupProbePath(),
	}
}

func (h *DockerRuntime) Stop(ctx context.Context, target Container) error {
	_, err := h.docker.ContainerStop(ctx, target.ID, client.ContainerStopOptions{})

	return h.mutationError("stop container", err)
}

func (h *DockerRuntime) Verify(ctx context.Context, target Container) error {
	ctx, cancel := context.WithTimeoutCause(ctx, verificationLimit, errVerificationBudget)
	defer cancel()
	deadline, _ := ctx.Deadline()

	for {
		// Do not start another inspection after the deadline. The timer's
		// cancellation may still need to propagate before its cause is set.
		if !time.Now().Before(deadline) {
			<-ctx.Done()

			return fmt.Errorf("wait for container health: %w", context.Cause(ctx))
		}

		inspected, err := h.docker.ContainerInspect(ctx, target.ID, client.ContainerInspectOptions{})
		if err != nil {
			// An unanswered inspection remains uncertain. Preserve both its
			// error and the context identity, even if the SDK returned only
			// the custom budget cause instead of DeadlineExceeded.
			return h.safeError(errors.Join(err, ctx.Err()))
		}
		state := inspected.Container.State
		if state == nil || !state.Running {
			if state == nil {
				return errors.New("container has no reported state")
			}

			return h.safeError(fmt.Errorf("container is %s with exit code %d", state.Status, state.ExitCode))
		}

		if state.Health == nil || string(state.Health.Status) == "healthy" {
			return nil
		}
		if string(state.Health.Status) == "unhealthy" {
			return errors.New("container health check reported unhealthy")
		}

		timer := time.NewTimer(verificationPoll)
		select {
		case <-ctx.Done():
			timer.Stop()

			// Reached only between successful inspections: the cause
			// distinguishes the gate's own expired budget, a definitive
			// answer, from the caller's cancellation or deadline.
			return fmt.Errorf("wait for container health: %w", context.Cause(ctx))
		case <-timer.C:
		}
	}
}

func (h *DockerRuntime) Logs(ctx context.Context, target Container) (string, error) {
	logs, err := h.docker.ContainerLogs(ctx, target.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "100",
	})
	if err != nil {
		return "", h.safeError(err)
	}
	defer logs.Close()

	var output bytes.Buffer
	limited := io.LimitReader(logs, maximumLogBytes)
	if _, err := stdcopy.StdCopy(&output, &output, limited); err != nil {
		return "", h.safeError(err)
	}

	return output.String(), nil
}

func (h *DockerRuntime) Remove(ctx context.Context, target Container) error {
	_, err := h.docker.ContainerRemove(ctx, target.ID, client.ContainerRemoveOptions{Force: true})

	return h.mutationError("remove container", err)
}

func containerCreateOptions(spec ContainerSpec, name string) client.ContainerCreateOptions {
	environment := make([]string, 0, len(spec.Environment))
	for key, value := range spec.Environment {
		environment = append(environment, key+"="+value)
	}

	slices.Sort(environment)
	config := &container.Config{
		Image:  spec.Image.ID,
		Cmd:    spec.Command,
		Env:    environment,
		Labels: containerLabels(spec),
	}
	hostConfig := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(spec.RestartPolicy)},
	}
	var networkingConfig *network.NetworkingConfig
	if spec.HTTP != nil {
		port := network.MustParsePort(fmt.Sprintf("%d/tcp", spec.HTTP.ContainerPort))
		config.ExposedPorts = network.PortSet{port: struct{}{}}
		networkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{workloadNetwork: {}},
		}
	}
	return client.ContainerCreateOptions{
		Name:             name,
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
	}
}

func managedContainerName(workload, suffix string) string {
	maximumWorkloadLength := maximumDNSLabel - len(suffix) - 1
	if len(workload) > maximumWorkloadLength {
		workload = strings.TrimRight(workload[:maximumWorkloadLength], "-")
	}

	return workload + "-" + suffix
}

// containerLabels records the metadata labels describing the configuration
// the container is created from. Labels carry references, never payloads.
func containerLabels(spec ContainerSpec) map[string]string {
	labels := map[string]string{
		ManagedLabel:  "true",
		WorkloadLabel: spec.Workload,
		KindLabel:     KindBackground,
	}
	if spec.HTTP != nil {
		labels[KindLabel] = KindHTTP
		labels[HTTPPortLabel] = strconv.FormatUint(uint64(spec.HTTP.ContainerPort), 10)
		labels[HTTPProbePathLabel] = spec.HTTP.StartupProbePath
	}

	if len(spec.SecretEnvironment) != 0 {
		references, _ := json.Marshal(spec.SecretEnvironment) // String maps are always JSON-encodable.
		labels[SecretEnvironmentLabel] = string(references)
	}

	return labels
}

// metadataMatches reports whether deployed labels record the desired
// configuration. Missing, malformed, or contradictory metadata never matches.
func metadataMatches(labels map[string]string, desired ContainerSpec) bool {
	metadata, ok := DecodeMetadata(labels)
	if !ok || labels[WorkloadLabel] != desired.Workload {
		return false
	}
	expectedKind := KindBackground
	if desired.HTTP != nil {
		expectedKind = KindHTTP
	}

	if metadata.Kind != expectedKind {
		return false
	}
	if desired.HTTP != nil &&
		(metadata.HTTPPort != desired.HTTP.ContainerPort || metadata.StartupProbePath != desired.HTTP.StartupProbePath) {
		return false
	}

	return maps.Equal(metadata.SecretEnvironment, desired.SecretEnvironment)
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		result[key] = value
	}

	return result
}

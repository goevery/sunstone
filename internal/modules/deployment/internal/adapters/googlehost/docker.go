package googlehost

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"time"

	"connectrpc.com/connect"
	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"
)

const (
	managedLabel       = "dev.sunstone.managed"
	workloadLabel      = "dev.sunstone.workload"
	configurationLabel = "dev.sunstone.configuration"
	verificationLimit  = 5 * time.Minute
	verificationPoll   = time.Second
	maximumLogBytes    = 64 * 1024
	workloadNetwork    = "sunstone"
	maximumDNSLabel    = 63
)

type host struct {
	docker           *client.Client
	ssh              *ssh.Client
	controlTransport *http.Transport
	routes           sunbeampbconnect.SunbeamClient
}

func (h *host) Close() error {
	h.controlTransport.CloseIdleConnections()
	return errors.Join(h.docker.Close(), h.ssh.Close())
}

func (h *host) Pull(ctx context.Context, reference string) (deploy.Image, error) {
	pull, err := h.docker.ImagePull(ctx, reference, client.ImagePullOptions{})
	if err != nil {
		return deploy.Image{}, err
	}
	defer pull.Close()
	if err := pull.Wait(ctx); err != nil {
		return deploy.Image{}, err
	}
	inspected, err := h.docker.ImageInspect(ctx, reference)
	if err != nil {
		return deploy.Image{}, err
	}
	image := deploy.Image{ID: inspected.ID}
	if inspected.Config != nil {
		image.DefaultCommand = slices.Clone(inspected.Config.Cmd)
		image.DefaultEnvironment = slices.Clone(inspected.Config.Env)
	}

	return image, nil
}

func (h *host) WorkloadContainers(ctx context.Context, workload string) ([]deploy.Container, error) {
	filters := make(client.Filters).
		Add("label", managedLabel+"=true").
		Add("label", workloadLabel+"="+workload)
	listed, err := h.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	containers := make([]deploy.Container, 0, len(listed.Items))
	for _, item := range listed.Items {
		name := ""
		if len(item.Names) != 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		healthy := item.State == container.StateRunning
		if item.Health != nil {
			healthy = string(item.Health.Status) == "healthy"
		}
		containers = append(containers, deploy.Container{
			ID:      item.ID,
			Name:    name,
			Image:   item.ImageID,
			Running: item.State == container.StateRunning,
			Healthy: healthy,
		})
	}

	return containers, nil
}

func (h *host) Matches(ctx context.Context, current deploy.Container, desired deploy.ContainerSpec) (bool, error) {
	inspected, err := h.docker.ContainerInspect(ctx, current.ID, client.ContainerInspectOptions{})
	if err != nil {
		return false, err
	}
	if inspected.Container.Config == nil || inspected.Container.HostConfig == nil {
		return false, errors.New("container has incomplete configuration")
	}
	expectedCommand := desired.Command
	if expectedCommand == nil {
		expectedCommand = desired.Image.DefaultCommand
	}
	expectedEnvironment := environmentMap(desired.Image.DefaultEnvironment)
	maps.Copy(expectedEnvironment, desired.Environment)
	actualEnvironment := environmentMap(inspected.Container.Config.Env)

	fingerprint, err := configurationFingerprint(desired)
	if err != nil {
		return false, err
	}
	if desired.HTTP != nil && (inspected.Container.NetworkSettings == nil || inspected.Container.NetworkSettings.Networks[workloadNetwork] == nil) {
		return false, nil
	}

	return inspected.Container.Image == desired.Image.ID &&
		slices.Equal(inspected.Container.Config.Cmd, expectedCommand) &&
		maps.Equal(actualEnvironment, expectedEnvironment) &&
		string(inspected.Container.HostConfig.RestartPolicy.Name) == desired.RestartPolicy &&
		inspected.Container.Config.Labels[configurationLabel] == fingerprint, nil
}

func (h *host) Create(ctx context.Context, spec deploy.ContainerSpec) (deploy.Container, error) {
	fingerprint, err := configurationFingerprint(spec)
	if err != nil {
		return deploy.Container{}, err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return deploy.Container{}, fmt.Errorf("generate container name: %w", err)
	}
	name := managedContainerName(spec.Name, fingerprint, hex.EncodeToString(suffix))
	created, err := h.docker.ContainerCreate(ctx, containerCreateOptions(spec, name, fingerprint))
	if err != nil {
		return deploy.Container{}, err
	}

	return deploy.Container{ID: created.ID, Name: name}, nil
}

func (h *host) Start(ctx context.Context, target deploy.Container) error {
	_, err := h.docker.ContainerStart(ctx, target.ID, client.ContainerStartOptions{})
	return err
}

func (h *host) BackendAddress(_ context.Context, target deploy.Container, containerPort uint16) (string, error) {
	if target.Name == "" {
		return "", errors.New("container has no network name")
	}
	return net.JoinHostPort(target.Name, strconv.FormatUint(uint64(containerPort), 10)), nil
}

func (h *host) Route(ctx context.Context, workload string) (deploy.Route, bool, error) {
	response, err := h.routes.GetRoute(ctx, connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/" + workload}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return deploy.Route{}, false, nil
	}
	if err != nil {
		return deploy.Route{}, false, err
	}
	return routeFromProto(response.Msg), true, nil
}

func (h *host) UpdateRoute(ctx context.Context, route deploy.Route) error {
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
	return err
}

func (h *host) DeleteRoute(ctx context.Context, workload string, allowMissing bool) error {
	_, err := h.routes.DeleteRoute(ctx, connect.NewRequest(&sunbeampb.DeleteRouteRequest{
		Name:         "routes/" + workload,
		AllowMissing: allowMissing,
	}))
	return err
}

func routeFromProto(route *sunbeampb.Route) deploy.Route {
	return deploy.Route{
		Workload:         strings.TrimPrefix(route.GetName(), "routes/"),
		Address:          route.GetBackend().GetAddress(),
		ContainerID:      route.GetBackend().GetContainerId(),
		StartupProbePath: route.GetBackend().GetStartupProbePath(),
	}
}

func (h *host) Stop(ctx context.Context, target deploy.Container) error {
	_, err := h.docker.ContainerStop(ctx, target.ID, client.ContainerStopOptions{})
	return err
}

func (h *host) Verify(ctx context.Context, target deploy.Container) error {
	ctx, cancel := context.WithTimeout(ctx, verificationLimit)
	defer cancel()

	for {
		inspected, err := h.docker.ContainerInspect(ctx, target.ID, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		state := inspected.Container.State
		if state == nil || !state.Running {
			if state == nil {
				return errors.New("container has no reported state")
			}

			return fmt.Errorf("container is %s with exit code %d", state.Status, state.ExitCode)
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
			return fmt.Errorf("wait for container health: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (h *host) Logs(ctx context.Context, target deploy.Container) (string, error) {
	logs, err := h.docker.ContainerLogs(ctx, target.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "100",
	})
	if err != nil {
		return "", err
	}
	defer logs.Close()

	var output bytes.Buffer
	limited := io.LimitReader(logs, maximumLogBytes)
	if _, err := stdcopy.StdCopy(&output, &output, limited); err != nil {
		return "", err
	}

	return output.String(), nil
}

func (h *host) Remove(ctx context.Context, target deploy.Container) error {
	_, err := h.docker.ContainerRemove(ctx, target.ID, client.ContainerRemoveOptions{Force: true})
	return err
}

func containerCreateOptions(spec deploy.ContainerSpec, name, fingerprint string) client.ContainerCreateOptions {
	environment := make([]string, 0, len(spec.Environment))
	for key, value := range spec.Environment {
		environment = append(environment, key+"="+value)
	}
	slices.Sort(environment)
	config := &container.Config{
		Image: spec.Image.ID,
		Cmd:   spec.Command,
		Env:   environment,
		Labels: map[string]string{
			managedLabel:       "true",
			workloadLabel:      spec.Workload,
			configurationLabel: fingerprint,
		},
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

func managedContainerName(workload, fingerprint, suffix string) string {
	maximumWorkloadLength := maximumDNSLabel - len(fingerprint[:12]) - len(suffix) - 2
	if len(workload) > maximumWorkloadLength {
		workload = strings.TrimRight(workload[:maximumWorkloadLength], "-")
	}
	return fmt.Sprintf("%s-%s-%s", workload, fingerprint[:12], suffix)
}

func configurationFingerprint(spec deploy.ContainerSpec) (string, error) {
	networkName := ""
	if spec.HTTP != nil {
		networkName = workloadNetwork
	}
	encoded, err := json.Marshal(struct {
		Image         string
		Command       []string
		Environment   map[string]string
		RestartPolicy string
		HTTP          *deploy.HTTPConfig `json:",omitempty"`
		Network       string             `json:",omitempty"`
	}{spec.Image.ID, spec.Command, spec.Environment, spec.RestartPolicy, spec.HTTP, networkName})
	if err != nil {
		return "", fmt.Errorf("encode container configuration: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		result[key] = value
	}

	return result
}

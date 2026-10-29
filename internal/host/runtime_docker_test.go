package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"github.com/goevery/sunstone/internal/workload"
)

// inspectStarting is a Docker inspect answer for a container that keeps
// running without ever becoming healthy.
const inspectStarting = `{"State":{"Status":"running","Running":true,"Health":{"Status":"starting"}}}`

// inspectAnswer describes one fake Docker inspect outcome: answering with
// a body, hanging without ever answering, or dropping the connection
// without answering.
type inspectAnswer struct {
	body string
	hang bool
	drop bool
}

// answeringInspection returns the given inspect body as the answer.
func answeringInspection(body string) inspectAnswer {
	return inspectAnswer{body: body}
}

// hungInspection accepts the request and never answers it.
func hungInspection() inspectAnswer {
	return inspectAnswer{hang: true}
}

// droppedInspection closes the connection without answering.
func droppedInspection() inspectAnswer {
	return inspectAnswer{drop: true}
}

// pipeSession adapts in-process pipe connections to the host Session seam,
// so a Docker runtime under test speaks to a hand-written Docker API without
// real sockets: goroutines parked on pipes let synctest bubbles advance fake
// time.
type pipeSession struct {
	serve func(net.Conn)
}

func (s pipeSession) DialContext(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go s.serve(server)

	return client, nil
}

// verifyRuntime builds a Docker runtime whose Docker API endpoint answers
// over in-process pipes. The inspect function receives the 1-based call
// number and returns how that inspection ends. The caller closes the
// returned runtime.
func verifyRuntime(t *testing.T, inspect func(call int) inspectAnswer) *DockerRuntime {
	t.Helper()
	runtime, err := NewDockerRuntime(pipeSession{serve: func(conn net.Conn) {
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for call := 1; ; call++ {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			answer := inspectAnswer{body: `{"message":"no such endpoint"}`}
			headers := ""
			switch {
			case strings.HasSuffix(request.URL.Path, "/_ping"):
				// The client pings with HEAD: only headers, never a body.
				headers = "API-Version: 1.47\r\n"
			case strings.HasSuffix(request.URL.Path, "/containers/candidate/json"):
				answer = inspect(call)
			}
			if request.Method != http.MethodHead {
				headers += fmt.Sprintf("Content-Length: %d\r\n", len(answer.body))
			}
			if answer.drop {
				// The connection is closed with the request unanswered.
				return
			}
			if answer.hang {
				// The daemon accepts the request and never answers; the
				// connection parks until the client aborts it.
				_, _ = io.Copy(io.Discard, conn)

				return
			}
			response := "HTTP/1.1 200 OK\r\n" + headers + "\r\n"
			if request.Method != http.MethodHead {
				response += answer.body
			}
			if _, err := conn.Write([]byte(response)); err != nil {
				return
			}
		}
	}}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	return runtime
}

func TestNewDockerRuntimeRequiresSession(t *testing.T) {
	if _, err := NewDockerRuntime(nil, testControlAddress); err == nil {
		t.Fatal("accepted a missing session")
	}
}

func TestNewDockerRuntimeRequiresControlAddress(t *testing.T) {
	if _, err := NewDockerRuntime(sshTransport{}, ""); err == nil {
		t.Fatal("accepted a missing control address")
	}
}

func TestHTTPContainerJoinsSunstoneNetworkWithoutHostPort(t *testing.T) {
	options := containerCreateOptions(ContainerSpec{
		HTTP: &workload.HTTPConfig{ContainerPort: 8080},
	}, "storefront-01234567")
	if options.NetworkingConfig == nil || options.NetworkingConfig.EndpointsConfig[workloadNetwork] == nil {
		t.Fatalf("networking config = %+v", options.NetworkingConfig)
	}
	if len(options.HostConfig.PortBindings) != 0 {
		t.Fatalf("host port bindings = %+v", options.HostConfig.PortBindings)
	}
	if len(options.Config.ExposedPorts) != 1 {
		t.Fatalf("exposed ports = %+v", options.Config.ExposedPorts)
	}
}

func TestCreateMarksEffectsUnconfirmedWhenAnswerIsLost(t *testing.T) {
	// The Docker API accepted the create request, then the connection was
	// dropped before any response: the daemon may have applied it, so the
	// failure is unconfirmed rather than a definitive rejection.
	f := newSecretHost(t)
	f.dropCreate.Store(true)

	_, err := f.host.Create(t.Context(), secretSpec())
	if !EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	if f.creates.Load() != 1 {
		t.Fatalf("accepted creates = %d", f.creates.Load())
	}
}

func TestCreateMarksEffectsUnconfirmedOnMalformedSuccessResponse(t *testing.T) {
	// The daemon accepts the create and answers success with a body the
	// client cannot decode: the unrecognized answer proves nothing about
	// whether the mutation was applied.
	f := newSecretHost(t)
	f.malformedCreate.Store(true)

	_, err := f.host.Create(t.Context(), secretSpec())
	if !EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	if f.creates.Load() != 1 {
		t.Fatalf("accepted creates = %d", f.creates.Load())
	}
}

func TestUpdateRouteMarksEffectsUnconfirmedWhenAnswerIsLost(t *testing.T) {
	// Sunbeam's control endpoint accepted the route update request, then
	// the connection was dropped before any response: the route may
	// already serve the new backend, so the failure is unconfirmed.
	f := newSecretHost(t)

	err := f.host.UpdateRoute(t.Context(), Route{Workload: "storefront", Address: "web:8080", ContainerID: "container-1"})
	if !EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
}

func TestRouteErrorClassifiesConservatively(t *testing.T) {
	// Only a genuine wire answer with a known rejection code proves
	// Sunbeam did not apply the change; a wire startup rejection stays
	// definitive so safe cleanup can proceed.
	wireRejections := []error{
		connect.NewWireError(connect.CodeFailedPrecondition, errors.New("backend failed startup probe: startup probe deadline exceeded")),
		connect.NewWireError(connect.CodeInvalidArgument, errors.New("invalid route")),
		connect.NewWireError(connect.CodeNotFound, errors.New("no such route")),
	}
	for _, rejection := range wireRejections {
		if classified := routeError("update route", rejection); EffectsUnconfirmed(classified) {
			t.Fatalf("wire rejection marked unconfirmed: %v", classified)
		}
	}

	// Wire server failures, transport-level codes, and local deadline
	// failures say nothing about the route's final state.
	uncertain := []error{
		connect.NewWireError(connect.CodeInternal, errors.New("route store corrupted")),
		connect.NewError(connect.CodeInternal, errors.New("locally synthesized")),
		connect.NewError(connect.CodeUnavailable, errors.New("unavailable")),
		fmt.Errorf("update route: %w", context.DeadlineExceeded),
	}
	for _, cause := range uncertain {
		classified := routeError("update route", cause)
		if !EffectsUnconfirmed(classified) {
			t.Fatalf("route failure treated as definitive: %v", cause)
		}
		if !errors.Is(classified, cause) {
			t.Fatalf("cause identity lost: %v", classified)
		}
	}
	if routeError("update route", nil) != nil {
		t.Fatal("nil error classified")
	}
}

func TestVerifyBudgetExhaustionIsDefinitive(t *testing.T) {
	// Every inspection answers, and the container keeps running without
	// becoming healthy for the whole five-minute budget. Fake time
	// advances the one-second polls without sleeping, and a budget that
	// expires between successful inspections is a known answer rather
	// than a lost transport, so safe cleanup can proceed after it.
	synctest.Test(t, func(t *testing.T) {
		runtime := verifyRuntime(t, func(int) inspectAnswer {
			return answeringInspection(inspectStarting)
		})
		defer runtime.Close()

		err := runtime.Verify(context.Background(), Container{ID: "candidate"})
		if !errors.Is(err, errVerificationBudget) {
			t.Fatalf("error = %v", err)
		}
		if transportLost(err) {
			t.Fatalf("budget exhaustion classified as lost transport: %v", err)
		}
	})
}

func TestVerifyInFlightInspectionExpiryStaysLostTransport(t *testing.T) {
	// The daemon stops answering once less than the client's per-request
	// timeout remains in the budget: the health budget itself expires the
	// in-flight inspect. That read went unanswered, so it is not the
	// gate's verdict — it keeps the no-cleanup policy for lost transport,
	// with the deadline identity restored even if the SDK surfaces the
	// budget cause instead of a deadline sentinel.
	synctest.Test(t, func(t *testing.T) {
		runtime := verifyRuntime(t, func(call int) inspectAnswer {
			// Polls run one second apart, so the 280th starts with about
			// twenty seconds of budget left, inside the client's 30-second
			// per-request timeout.
			if call == 280 {
				return hungInspection()
			}

			return answeringInspection(inspectStarting)
		})
		defer runtime.Close()

		err := runtime.Verify(context.Background(), Container{ID: "candidate"})
		if err == nil || !transportLost(err) {
			t.Fatalf("expired in-flight inspection classified as definitive: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline identity lost from error: %v", err)
		}
	})
}

func TestVerifyLostInspectionStaysLostTransport(t *testing.T) {
	// The connection carrying the first inspection is dropped before any
	// answer: the health gate has no verdict of its own, so the failure
	// keeps the no-cleanup policy for lost transport.
	synctest.Test(t, func(t *testing.T) {
		runtime := verifyRuntime(t, func(int) inspectAnswer {
			return droppedInspection()
		})
		defer runtime.Close()

		err := runtime.Verify(context.Background(), Container{ID: "candidate"})
		if err == nil || !transportLost(err) {
			t.Fatalf("dropped inspection classified as definitive: %v", err)
		}
		if errors.Is(err, errVerificationBudget) {
			t.Fatalf("lost inspection reported as budget answer: %v", err)
		}
	})
}

func TestVerifyCallerCancellationStaysLostTransport(t *testing.T) {
	// The daemon hangs the first inspect and the caller cancels mid-wait:
	// the health gate has no answer of its own, so the failure keeps the
	// no-cleanup policy for lost transport.
	synctest.Test(t, func(t *testing.T) {
		runtime := verifyRuntime(t, func(int) inspectAnswer {
			return hungInspection()
		})
		defer runtime.Close()

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Second, cancel)
		err := runtime.Verify(ctx, Container{ID: "candidate"})
		if err == nil || !transportLost(err) {
			t.Fatalf("caller cancellation classified as definitive: %v", err)
		}
		if errors.Is(err, errVerificationBudget) {
			t.Fatalf("inherited cancellation reported as budget answer: %v", err)
		}
	})
}

func TestVerifyCallerDeadlineStaysLostTransport(t *testing.T) {
	// The caller's own deadline, shorter than the health budget, is an
	// unknown local RPC deadline: the gate has no answer of its own, so
	// the failure keeps the no-cleanup policy for lost transport.
	synctest.Test(t, func(t *testing.T) {
		runtime := verifyRuntime(t, func(int) inspectAnswer {
			return answeringInspection(inspectStarting)
		})
		defer runtime.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := runtime.Verify(ctx, Container{ID: "candidate"})
		if err == nil || !transportLost(err) {
			t.Fatalf("caller deadline classified as definitive: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline lost from error: %v", err)
		}
		if errors.Is(err, errVerificationBudget) {
			t.Fatalf("inherited deadline reported as budget answer: %v", err)
		}
	})
}

func TestHTTPBackendUsesManagedContainerNetworkName(t *testing.T) {
	target := Container{Name: "storefront-01234567"}
	address, err := (&DockerRuntime{}).BackendAddress(t.Context(), target, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if address != "storefront-01234567:8080" {
		t.Fatalf("backend address = %q", address)
	}
}

func TestManagedContainerNameIsDNSLabel(t *testing.T) {
	workload := "a-very-long-workload-name-that-uses-the-maximum-allowed-label-length"
	name := managedContainerName(workload, "01234567")
	if len(name) > maximumDNSLabel {
		t.Fatalf("container name has length %d: %q", len(name), name)
	}
	if !strings.HasPrefix(name, strings.TrimRight(workload[:maximumDNSLabel-len("01234567")-1], "-")+"-") {
		t.Fatalf("container name = %q", name)
	}
	if !strings.HasSuffix(name, "-01234567") {
		t.Fatalf("container name = %q", name)
	}
	if strings.Contains(name, "sha256") || strings.Contains(name, "super-secret") {
		t.Fatalf("container name exposes configuration: %q", name)
	}
}

func TestBackgroundMetadataLabels(t *testing.T) {
	labels := containerLabels(ContainerSpec{
		Workload:      "jobs",
		Image:         Image{ID: "sha256:image"},
		Environment:   map[string]string{"PASSWORD": "super-secret"},
		RestartPolicy: "unless-stopped",
	})
	if labels[ManagedLabel] != "true" || labels[WorkloadLabel] != "jobs" || labels[KindLabel] != KindBackground {
		t.Fatalf("background labels = %v", labels)
	}
	if labels[HTTPPortLabel] != "" || labels[HTTPProbePathLabel] != "" || labels[SecretEnvironmentLabel] != "" {
		t.Fatalf("background labels carry HTTP or secret data: %v", labels)
	}
	encoded, _ := json.Marshal(labels)
	if strings.Contains(string(encoded), "super-secret") {
		t.Fatal("labels expose environment values")
	}
}

func TestHTTPMetadataLabels(t *testing.T) {
	labels := containerLabels(ContainerSpec{
		Workload:          "storefront",
		HTTP:              &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
		SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/latest"},
	})
	if labels[ManagedLabel] != "true" || labels[WorkloadLabel] != "storefront" {
		t.Fatalf("identity labels = %v", labels)
	}
	if labels[KindLabel] != KindHTTP || labels[HTTPPortLabel] != "8080" || labels[HTTPProbePathLabel] != "/readyz" {
		t.Fatalf("HTTP labels = %v", labels)
	}
	var references map[string]string
	if err := json.Unmarshal([]byte(labels[SecretEnvironmentLabel]), &references); err != nil {
		t.Fatalf("secret-env label is not JSON: %q", labels[SecretEnvironmentLabel])
	}
	if len(references) != 1 || references["API_KEY"] != "projects/p/secrets/key/versions/latest" {
		t.Fatalf("secret references = %v", references)
	}
	encoded, _ := json.Marshal(labels)
	if strings.Contains(string(encoded), "vm-secret-value") {
		t.Fatal("labels expose resolved secret payloads")
	}
}

func TestMetadataMatchesCurrentConfiguration(t *testing.T) {
	desired := ContainerSpec{
		Workload:          "storefront",
		Image:             Image{ID: "sha256:image"},
		RestartPolicy:     "unless-stopped",
		HTTP:              &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
		SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"},
	}
	if !metadataMatches(containerLabels(desired), desired) {
		t.Fatal("freshly recorded metadata did not match its own configuration")
	}
}

func TestMetadataMatchIgnoresSecretReferenceJSONOrdering(t *testing.T) {
	desired := ContainerSpec{
		Workload:          "jobs",
		SecretEnvironment: map[string]string{"A": "projects/p/secrets/a/versions/1", "B": "projects/p/secrets/b/versions/1"},
	}
	labels := containerLabels(desired)
	labels[SecretEnvironmentLabel] = `{"B":"projects/p/secrets/b/versions/1","A":"projects/p/secrets/a/versions/1"}`
	if !metadataMatches(labels, desired) {
		t.Fatal("secret reference ordering changed the match result")
	}
}

func TestMetadataMatchRequiresAgreedMetadata(t *testing.T) {
	background := ContainerSpec{Workload: "jobs", Image: Image{ID: "sha256:image"}, RestartPolicy: "unless-stopped"}
	httpSpec := ContainerSpec{
		Workload:      "storefront",
		RestartPolicy: "unless-stopped",
		HTTP:          &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
	}
	legacy := map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs"}
	legacyHashed := map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs", "dev.sunstone.configuration": "a567ec426a494c90501fe4577041ae3bf2dedcd84e11371e52d720bf00f463b4"}

	tests := []struct {
		name    string
		labels  map[string]string
		desired ContainerSpec
	}{
		{name: "legacy without kind", labels: legacy, desired: background},
		{name: "legacy configuration hash", labels: legacyHashed, desired: background},
		{name: "missing managed", labels: map[string]string{WorkloadLabel: "jobs", KindLabel: KindBackground}, desired: background},
		{name: "unknown kind", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs", KindLabel: "cron"}, desired: background},
		{name: "background with HTTP fields", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs", KindLabel: KindBackground, HTTPPortLabel: "8080"}, desired: background},
		{name: "http without port", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "storefront", KindLabel: KindHTTP, HTTPProbePathLabel: "/readyz"}, desired: httpSpec},
		{name: "http without probe", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "storefront", KindLabel: KindHTTP, HTTPPortLabel: "8080"}, desired: httpSpec},
		{name: "http with zero port", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "storefront", KindLabel: KindHTTP, HTTPPortLabel: "0", HTTPProbePathLabel: "/readyz"}, desired: httpSpec},
		{name: "malformed secret references", labels: map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs", KindLabel: KindBackground, SecretEnvironmentLabel: "{not-json"}, desired: background},
	}
	for _, test := range tests {
		if metadataMatches(test.labels, test.desired) {
			t.Fatalf("%s: malformed or missing metadata matched", test.name)
		}
	}
}

func TestMetadataMatchDetectsConfigurationDrift(t *testing.T) {
	base := ContainerSpec{
		Workload:          "jobs",
		Image:             Image{ID: "sha256:image"},
		RestartPolicy:     "unless-stopped",
		Environment:       map[string]string{"LITERAL": "keep"},
		SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"},
	}
	httpSpec := ContainerSpec{
		Workload:          "storefront",
		RestartPolicy:     "unless-stopped",
		HTTP:              &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
		SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"},
	}
	tests := []struct {
		name    string
		labels  map[string]string
		desired ContainerSpec
	}{
		{name: "workload", labels: containerLabels(base), desired: withWorkload(base, "other")},
		{name: "kind", labels: containerLabels(base), desired: httpSpec},
		{name: "http port", labels: containerLabels(httpSpec), desired: withHTTPPort(httpSpec, 9090)},
		{name: "probe path", labels: containerLabels(httpSpec), desired: withHTTPProbe(httpSpec, "/healthz")},
		{name: "secret reference", labels: containerLabels(base), desired: withSecretRef(base, "API_KEY", "projects/p/secrets/key/versions/2")},
		{name: "added secret reference", labels: containerLabels(base), desired: withAddedSecretRef(base, "DATABASE", "projects/p/secrets/db/versions/1")},
		{name: "removed secret reference", labels: containerLabels(withAddedSecretRef(base, "DATABASE", "projects/p/secrets/db/versions/1")), desired: base},
	}
	for _, test := range tests {
		if metadataMatches(test.labels, test.desired) {
			t.Fatalf("%s: configuration drift matched", test.name)
		}
	}
}

func TestMatchesEffectiveCommandEquivalence(t *testing.T) {
	defaulted := secretSpec()
	defaulted.Image.DefaultCommand = []string{"bin/jobs"}

	tests := []struct {
		name    string
		desired ContainerSpec
	}{
		{name: "omitted command uses image default", desired: defaulted},
		{name: "explicit command equals image default", desired: withCommand(defaulted, []string{"bin/jobs"})},
	}
	for _, test := range tests {
		f := newSecretHost(t)
		f.inspectCmd.Store([]string{"bin/jobs"})
		candidate, err := f.host.Create(t.Context(), test.desired)
		if err != nil {
			t.Fatal(err)
		}

		matches, err := f.host.Matches(t.Context(), candidate, test.desired)
		if err != nil {
			t.Fatal(err)
		}
		if !matches {
			t.Fatalf("%s: equivalent effective command did not match", test.name)
		}
	}
}

func TestMatchesDetectsCommandDrift(t *testing.T) {
	f := newSecretHost(t)
	f.inspectCmd.Store([]string{"bin/jobs"})
	desired := withCommand(secretSpec(), []string{"bin/jobs"})
	candidate, err := f.host.Create(t.Context(), desired)
	if err != nil {
		t.Fatal(err)
	}

	desired = withCommand(desired, []string{"bin/new"})
	matches, err := f.host.Matches(t.Context(), candidate, desired)
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("changed command matched")
	}
}

func TestMatchesDetectsRuntimeConfigurationDrift(t *testing.T) {
	drift := func(t *testing.T, name string, apply func(f *secretHostFixture)) {
		t.Helper()
		f := newSecretHost(t)
		spec := secretSpec()
		candidate, err := f.host.Create(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		apply(f)

		matches, err := f.host.Matches(t.Context(), candidate, spec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if matches {
			t.Fatalf("%s: drift matched", name)
		}
	}

	drift(t, "image", func(f *secretHostFixture) { f.inspectImage.Store("sha256:other") })
	drift(t, "environment", func(f *secretHostFixture) {
		f.mu.Lock()
		f.created.Config.Env["EXTRA"] = "unmanaged"
		f.mu.Unlock()
	})
	drift(t, "restart policy", func(f *secretHostFixture) { f.inspectRestart.Store("always") })
}

func TestMatchesRequiresHTTPNetworkAttachment(t *testing.T) {
	f := newSecretHost(t)
	spec := secretSpec()
	spec.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	candidate, err := f.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}

	matches, err := f.host.Matches(t.Context(), candidate, spec)
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("HTTP container matched without the managed network")
	}

	f.inspectNetwork.Store(true)
	matches, err = f.host.Matches(t.Context(), candidate, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("HTTP container with the managed network did not match")
	}
}

func TestWorkloadContainersDecodesDeployedMetadataFromListLabels(t *testing.T) {
	httpSpec := ContainerSpec{
		Workload: "storefront",
		HTTP:     &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
		SecretEnvironment: map[string]string{
			"API_KEY": "projects/p/secrets/key/versions/latest",
		},
	}
	f := newSecretHost(t)
	f.mu.Lock()
	f.listed = []map[string]any{
		{
			"Id":      "http-container",
			"Names":   []string{"/storefront-01234567"},
			"ImageID": "sha256:http-image",
			"Labels":  containerLabels(httpSpec),
			"State":   "running",
			"Health":  map[string]any{"Status": "healthy"},
		},
		{
			"Id":      "background-container",
			"Names":   []string{"/jobs-01234567"},
			"ImageID": "sha256:jobs-image",
			"Labels":  containerLabels(ContainerSpec{Workload: "jobs"}),
			"State":   "exited",
		},
		{
			"Id":      "legacy-container",
			"Names":   []string{"/legacy"},
			"ImageID": "sha256:legacy-image",
			"Labels":  map[string]string{ManagedLabel: "true", WorkloadLabel: "jobs", "dev.sunstone.configuration": "a567ec426a494c90501fe4577041ae3bf2dedcd84e11371e52d720bf00f463b4"},
			"State":   "running",
		},
	}
	f.mu.Unlock()

	containers, err := f.host.WorkloadContainers(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 3 {
		t.Fatalf("containers = %+v", containers)
	}

	httpContainer := containers[0]
	if httpContainer.ID != "http-container" || httpContainer.Name != "storefront-01234567" ||
		httpContainer.Image != "sha256:http-image" || !httpContainer.Running || !httpContainer.Healthy {
		t.Fatalf("HTTP container = %+v", httpContainer)
	}
	if httpContainer.Metadata == nil || httpContainer.Metadata.Kind != KindHTTP ||
		httpContainer.Metadata.HTTPPort != 8080 || httpContainer.Metadata.StartupProbePath != "/readyz" ||
		httpContainer.Metadata.SecretEnvironment["API_KEY"] != "projects/p/secrets/key/versions/latest" {
		t.Fatalf("HTTP metadata = %+v", httpContainer.Metadata)
	}

	backgroundContainer := containers[1]
	if backgroundContainer.ID != "background-container" || backgroundContainer.Running || backgroundContainer.Healthy {
		t.Fatalf("background container = %+v", backgroundContainer)
	}
	if backgroundContainer.Metadata == nil || backgroundContainer.Metadata.Kind != KindBackground ||
		backgroundContainer.Metadata.HTTPPort != 0 || backgroundContainer.Metadata.StartupProbePath != "" {
		t.Fatalf("background metadata = %+v", backgroundContainer.Metadata)
	}

	legacyContainer := containers[2]
	if legacyContainer.Metadata != nil {
		t.Fatalf("legacy metadata decoded = %+v", legacyContainer.Metadata)
	}
}

func withWorkload(spec ContainerSpec, workload string) ContainerSpec {
	spec.Workload = workload

	return spec
}

func withHTTPPort(spec ContainerSpec, port uint16) ContainerSpec {
	spec.HTTP.ContainerPort = port

	return spec
}

func withHTTPProbe(spec ContainerSpec, path string) ContainerSpec {
	spec.HTTP.StartupProbePath = path

	return spec
}

func withSecretRef(spec ContainerSpec, name, reference string) ContainerSpec {
	spec.SecretEnvironment = copiedRefs(spec.SecretEnvironment)
	spec.SecretEnvironment[name] = reference

	return spec
}

func withAddedSecretRef(spec ContainerSpec, name, reference string) ContainerSpec {
	spec.SecretEnvironment = copiedRefs(spec.SecretEnvironment)
	spec.SecretEnvironment[name] = reference

	return spec
}

func copiedRefs(references map[string]string) map[string]string {
	copied := make(map[string]string, len(references)+1)
	for key, value := range references {
		copied[key] = value
	}

	return copied
}

func withCommand(spec ContainerSpec, command []string) ContainerSpec {
	spec.Command = command

	return spec
}

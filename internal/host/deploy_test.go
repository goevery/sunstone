package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/goevery/sunstone/internal/workload"
)

// testHostConfig bounds interruption cleanup in host tests.
var testHostConfig = HostConfig{CleanupTimeout: 30 * time.Second}

func TestNewHostRejectsMissingCollaborators(t *testing.T) {
	if _, err := NewHost(nil, silentReporter(), testHostConfig); err == nil {
		t.Fatal("accepted a missing runtime")
	}
	if _, err := NewHost(&fakeRuntime{}, nil, testHostConfig); err == nil {
		t.Fatal("accepted a missing progress reporter")
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewHost(&fakeRuntime{}, silentReporter(), HostConfig{CleanupTimeout: timeout}); err == nil {
			t.Fatalf("accepted cleanup timeout %v", timeout)
		}
	}
}

func TestNewHostAcceptsExplicitCollaborators(t *testing.T) {
	runtime := &fakeRuntime{pullFunc: func(context.Context, string) (Image, error) {
		return Image{}, errors.New("unused")
	}}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}
	if vm == nil {
		t.Fatal("host = nil")
	}
}

func TestCurrentBackgroundWorkloadIsNotReplaced(t *testing.T) {
	current := Container{ID: "current", Name: "storefront-jobs", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return true, nil
		},
	}

	vm, facts := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Changed || outcome.Container != current.ID {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	assertFacts(t, facts, []Progress{{Stage: StagePulling, Image: "docker.io/example/jobs:v1"}, {Stage: StageCurrent}})
}

func TestChangedBackgroundWorkloadStopsBeforeStartingReplacement(t *testing.T) {
	current := Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	replacement := Container{ID: "new", Name: "storefront-jobs"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return replacement, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop-old")

			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			events = append(events, "verify-new")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, facts := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed {
		t.Fatal("changed deployment reported no change")
	}
	assertStrings(t, events, []string{"create", "stop-old", "start-new", "verify-new", "remove-old"})
	assertFacts(t, facts, []Progress{{Stage: StagePulling, Image: "docker.io/example/jobs:v1"}, {Stage: StageHealthy}})
}

func TestFailedReplacementRestartsPreviousRunningContainer(t *testing.T) {
	previous := Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	replacement := Container{ID: "new", Name: "storefront-jobs-new"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return replacement, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop-old")

			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return errors.New("unhealthy")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "crash", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "replacement logs:\ncrash") {
		t.Fatalf("expected failed replacement diagnostics, got %v", err)
	}
	assertStrings(t, events, []string{"stop-old", "start-new", "remove-new", "start-old"})
}

func TestHealthBudgetExhaustionRollsBackFailedReplacement(t *testing.T) {
	// The health gate exhausted its own waiting budget while the
	// replacement kept running without becoming healthy: the failure is a
	// known answer, not a lost transport, so the failed replacement is
	// removed and the previous container is rolled back.
	previous := Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	replacement := Container{ID: "new", Name: "storefront-jobs-new"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return replacement, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop-old")

			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return fmt.Errorf("wait for container health: %w", errVerificationBudget)
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || EffectsUnconfirmed(err) {
		t.Fatalf("expected a definitive failure, got %v", err)
	}
	assertStrings(t, events, []string{"stop-old", "start-new", "remove-new", "start-old"})
}

func TestFailedReplacementDoesNotRestartPreviouslyCrashedContainer(t *testing.T) {
	crashed := Container{ID: "old", Name: "storefront-jobs-old", Metadata: &DeployedMetadata{Kind: KindBackground}}
	replacement := Container{ID: "new", Name: "storefront-jobs"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{crashed}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return replacement, nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)

			return errors.New("start failed")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	if _, err := vm.Deploy(t.Context(), backgroundDefinition()); err == nil {
		t.Fatal("expected failed replacement")
	}
	assertStrings(t, events, []string{"start-new", "remove-new"})
}

func TestDeployRefusesAmbiguousContainers(t *testing.T) {
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "a"}, {ID: "b"}}, nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "refusing ambiguous state") {
		t.Fatalf("expected ambiguous-state refusal, got %v", err)
	}
}

func TestDeployBackgroundRefusesUnclassifiableCurrentContainer(t *testing.T) {
	// A managed container without valid metadata may be a serving HTTP
	// container behind a live route: a background deploy would stop and
	// remove it without retiring the route, leaving Sunbeam pointing at a
	// dead backend. The refusal happens before any mutation — the pull,
	// create, start, stop, remove, and route-mutation functions stay unset
	// and panic if reached — so the current container and its deployed
	// route stay exactly as observed.
	current := Container{ID: "legacy", Running: true, Healthy: true}
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			// A live route stands in for a prior HTTP deployment whose
			// metadata could not be classified.
			return Route{Workload: "storefront-jobs", Address: "legacy:8080", ContainerID: "legacy"}, true, nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "container legacy has missing or invalid deployed metadata; refusing background deployment") {
		t.Fatalf("error = %v", err)
	}
}

// Missing metadata requires replacement, but explicit HTTP routing makes
// repair safe: the route switches before the old container is removed.
func TestDeployHTTPRepairsMissingMetadataBeforeRemovingPreviousContainer(t *testing.T) {
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	previous := Container{ID: "old", Running: true, Healthy: true}
	replacement := Container{ID: "new"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) { return Image{ID: "image"}, nil },
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) { return false, nil },
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return replacement, nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start-new")

			return nil
		},
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "storefront-01234567:8080", nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			events = append(events, "route")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, facts := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed || outcome.Container != replacement.ID {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"create", "start-new", "route", "remove-old"})
	assertFacts(t, facts, []Progress{
		{Stage: StagePulling, Image: "docker.io/example/jobs:v1"},
		{Stage: StageStarting},
		{Stage: StageRouting},
		{Stage: StageDrained},
		{Stage: StageHealthy},
	})
}

func TestCurrentHTTPWorkloadAndRouteAreNotReplaced(t *testing.T) {
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	current := Container{ID: "current", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080, StartupProbePath: "/readyz"}}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) { return Image{ID: "image"}, nil },
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) { return true, nil },
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "storefront-01234567:8080", nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{Workload: definition.Name, Address: "storefront-01234567:8080", ContainerID: current.ID, StartupProbePath: "/readyz"}, true, nil
		},
	}

	vm, _ := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Changed || outcome.Container != current.ID {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
}

func TestRepairsHTTPRouteWithoutReplacingCurrentContainer(t *testing.T) {
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	current := Container{ID: "current", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080, StartupProbePath: "/readyz"}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) { return Image{ID: "image"}, nil },
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) { return true, nil },
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "storefront-01234567:8080", nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			events = append(events, "route")

			return nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return Container{}, nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed || outcome.Container != current.ID {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	assertStrings(t, events, []string{"route"})
}

func TestFailedHTTPRouteUpdateDoesNotRestartStoppedPreviousContainer(t *testing.T) {
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	previous := Container{ID: "old"}
	replacement := Container{ID: "new"}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) { return Image{ID: "image"}, nil },
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) { return false, nil },
		createFunc:  func(context.Context, ContainerSpec) (Container, error) { return replacement, nil },
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)

			return nil
		},
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "storefront-01234567:8080", nil
		},
		updateRouteFunc: func(context.Context, Route) error { return errors.New("not ready") },
		logsFunc:        func(context.Context, Container) (string, error) { return "booting", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "update route replacement: not ready") || !strings.Contains(err.Error(), "booting") {
		t.Fatalf("expected route diagnostics, got %v", err)
	}
	assertStrings(t, events, []string{"start-new", "remove-new"})
}

func TestHostFactsCarryNoTargetIdentity(t *testing.T) {
	// Orchestration enriches facts with workload and instance identity; the
	// per-VM host itself must not invent or require it.
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "current", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return true, nil
		},
	}

	vm, facts := newHost(t, runtime)
	if _, err := vm.Deploy(t.Context(), backgroundDefinition()); err != nil {
		t.Fatal(err)
	}
	for _, fact := range *facts {
		if fact.Workload != "" || fact.Instance != "" {
			t.Fatalf("host emitted target identity: %+v", fact)
		}
	}
}

func TestDeploySkipsCleanupWhenStartAnswerIsLost(t *testing.T) {
	// The start request may have been applied remotely: no logs, removal,
	// or rollback may run on its account. The logs and remove functions
	// stay unset and panic if reached.
	previous := Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		stopFunc: func(context.Context, Container) error {
			return nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start-attempted")

			return unconfirmed("start container", errors.New("Post \"http://docker\": EOF"))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "start container") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, []string{"start-attempted"})
}

func TestDeployHTTPKeepsReplacementWhenRouteUpdateAnswerIsLost(t *testing.T) {
	// An unacknowledged route update may already serve traffic: the
	// replacement must never be removed and the previous container must
	// never be drained on its account. The logs and remove functions stay
	// unset and panic if reached.
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "old", Running: true}}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		startFunc: func(context.Context, Container) error {
			return nil
		},
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "new:8080", nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			return unconfirmed("update route", errors.New("unavailable: EOF"))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), definition)
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "update route") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployCleanupHaltsWhenLogsAreLost(t *testing.T) {
	// A definitive verify failure starts safe cleanup; losing the
	// transport while fetching logs stops the remaining remote
	// operations. The remove function stays unset and panics if reached.
	previous := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	var started []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		stopFunc: func(context.Context, Container) error {
			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			started = append(started, container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return errors.New("unhealthy")
		},
		logsFunc: func(context.Context, Container) (string, error) {
			return "", io.EOF
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, started, []string{"new"})
}

func TestDeployCleanupHaltsWhenRemoveIsLost(t *testing.T) {
	// Losing the transport while removing the failed replacement stops the
	// rollback: restarting the previous container could race the dying
	// connection's actual state.
	previous := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	var started []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		stopFunc: func(context.Context, Container) error {
			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			started = append(started, container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return errors.New("unhealthy")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(context.Context, Container) error {
			return io.EOF
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	// Only the failed replacement was started; the previous container was
	// never restarted after the interrupted cleanup.
	assertStrings(t, started, []string{"new"})
}

func TestDeploySkipsSpeculativeCleanupWhenStopAnswerIsLost(t *testing.T) {
	// The stop of the previous container may have been applied: removing
	// the replacement could leave nothing running. The remove function
	// stays unset and panics if reached.
	previous := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		stopFunc: func(context.Context, Container) error {
			return unconfirmed("stop container", errors.New("read: connection reset"))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "stop current container") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployVerifyLossMarksFinalStateUnconfirmed(t *testing.T) {
	// The replacement was created and started with confirmed answers, but
	// the verification read lost the transport: the final state is
	// unconfirmed and no remote cleanup may run. The logs and remove
	// functions stay unset and panic if reached.
	var started []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		startFunc: func(_ context.Context, container Container) error {
			started = append(started, container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return fmt.Errorf("inspect container: %w", io.ErrUnexpectedEOF)
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "verify replacement") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, started, []string{"new"})
}

func TestDeployHTTPKeepsReplacementOnUnclassifiableRouteFailure(t *testing.T) {
	// A wire internal failure from Sunbeam says nothing about the route's
	// final state: no logs, removal, or rollback may run. The logs and
	// remove functions stay unset and panic if reached.
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "old", Running: true}}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		startFunc: func(context.Context, Container) error { return nil },
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "new:8080", nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			return routeError("update route", connect.NewWireError(connect.CodeInternal, errors.New("route store corrupted")))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), definition)
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "route store corrupted") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployCleanupStopsOnUnconfirmedRemoveWithServerFailure(t *testing.T) {
	// The remove during safe cleanup fails with a typed marker whose cause
	// is a server failure, not a network one: the rollback must not run and
	// the marker and its cause must survive.
	previous := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	var started []string
	removeCause := errors.New("daemon internal error")
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{previous}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		stopFunc: func(context.Context, Container) error { return nil },
		startFunc: func(_ context.Context, container Container) error {
			started = append(started, container.ID)

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return errors.New("unhealthy")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(context.Context, Container) error {
			return unconfirmed("remove container", removeCause)
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !errors.Is(err, removeCause) {
		t.Fatalf("error = %v", err)
	}
	// Only the failed replacement was started; the previous container was
	// never restarted after the uncertain cleanup.
	assertStrings(t, started, []string{"new"})
}

func TestDeployStartOfStoppedCurrentMarksStateUnconfirmedWhenVerificationLost(t *testing.T) {
	// The stopped-but-matching container starts with a confirmed answer,
	// then the verification read loses the transport: the final state is
	// unconfirmed.
	current := Container{ID: "current", Running: false, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v1"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return true, nil
		},
		startFunc: func(context.Context, Container) error { return nil },
		verifyFunc: func(context.Context, Container) error {
			return fmt.Errorf("inspect container: %w", io.EOF)
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "verify started container") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployHTTPReplacesUnhealthyCurrentWorkload(t *testing.T) {
	// A matching but unhealthy container is not current: it takes the
	// normal readiness-gated replacement instead of a route repair.
	current := Container{ID: "old", Running: true, Healthy: false}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return true, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return Container{ID: "new"}, nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start")

			return nil
		},
		backendAddressFunc: func(context.Context, Container, uint16) (string, error) {
			return "new:8080", nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			events = append(events, "route")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	definition := backgroundDefinition()
	definition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}

	vm, facts := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed || outcome.Container != "new" {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"create", "start", "route", "remove-old"})
	for _, fact := range *facts {
		if fact.Stage == StageCurrent {
			t.Fatalf("unhealthy workload reported current: %+v", *facts)
		}
	}
}

func TestDeployBackgroundRetiresRouteWhenTransitioningFromHTTP(t *testing.T) {
	// The deployed HTTP container keeps its route while the background
	// replacement is created; the route is drained and deleted before the
	// previous container stops, and a successful transition leaves it
	// deleted. A pure background deployment never contacts Sunbeam (the
	// route functions stay unset there).
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return Container{ID: "new"}, nil
		},
		routeFunc: func(_ context.Context, workloadName string) (Route, bool, error) {
			events = append(events, "route-get")

			return Route{Workload: workloadName, Address: "old:8080", ContainerID: "old"}, true, nil
		},
		deleteRouteFunc: func(_ context.Context, workloadName string, allowMissing bool) error {
			events = append(events, "route-delete")
			if !allowMissing && workloadName != "storefront-jobs" {
				t.Errorf("delete route = %q, %t", workloadName, allowMissing)
			}

			return nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop-old")

			return nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start-new")

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			events = append(events, "verify-new")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	outcome, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed || outcome.Container != "new" {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{
		"create", "route-get", "route-delete", "stop-old", "start-new", "verify-new", "remove-old",
	})
}

func TestDeployBackgroundTransitionRestoresRouteAfterDefinitiveStartupFailure(t *testing.T) {
	// A definitive startup failure after the route was retired rolls the
	// previous container back and reconnects its captured route — only
	// after the previous container starts.
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			events = append(events, "create")

			return Container{ID: "new"}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{Workload: "storefront-jobs", Address: "old:8080", ContainerID: "old"}, true, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			events = append(events, "route-delete")

			return nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop-old")

			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)
			if container.ID == "new" {
				return errors.New("start failed")
			}

			return nil
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
		updateRouteFunc: func(_ context.Context, route Route) error {
			events = append(events, "route-restore:"+route.ContainerID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "start replacement") || EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, []string{
		"create", "route-delete", "stop-old", "start-new", "remove-new", "start-old", "route-restore:old",
	})
}

func TestDeployBackgroundTransitionWithoutDeployedRoute(t *testing.T) {
	// No route was active, so none is retired and none is invented on
	// rollback. The delete and update functions stay unset and panic if
	// the deployment touches Sunbeam for this background workload.
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, nil
		},
		stopFunc: func(context.Context, Container) error {
			return nil
		},
		startFunc: func(_ context.Context, container Container) error {
			events = append(events, "start-"+container.ID)
			if container.ID == "new" {
				return errors.New("start failed")
			}

			return nil
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "start replacement") {
		t.Fatalf("error = %v", err)
	}
	// The rollback restarted the previous container without inventing a
	// route for it.
	assertStrings(t, events, []string{"start-new", "remove-new", "start-old"})
}

func TestDeployBackgroundTransitionRouteReadLossSkipsCleanup(t *testing.T) {
	// Reading the deployed route lost the connection after the candidate
	// existed: no remote cleanup may run — the remove, stop, and start
	// functions stay unset and panic if the deployment touches anything.
	for name, readErr := range map[string]error{
		"EOF":       fmt.Errorf("read containers: %w", io.EOF),
		"cancelled": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP}}
			runtime := &fakeRuntime{
				pullFunc: func(context.Context, string) (Image, error) {
					return Image{ID: "sha256:image-v2"}, nil
				},
				workloadContainersFunc: func(context.Context, string) ([]Container, error) {
					return []Container{current}, nil
				},
				matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
					return false, nil
				},
				createFunc: func(context.Context, ContainerSpec) (Container, error) {
					return Container{ID: "new"}, nil
				},
				routeFunc: func(context.Context, string) (Route, bool, error) {
					return Route{}, false, readErr
				},
			}

			vm, _ := newHost(t, runtime)
			_, err := vm.Deploy(t.Context(), backgroundDefinition())
			if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "get deployed route") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "remove") {
				t.Fatalf("uncertain route read triggered remote cleanup: %v", err)
			}
		})
	}
}

func TestDeployBackgroundTransitionDeleteUncertaintyLeavesEverything(t *testing.T) {
	// The route delete answer was lost: it may have been applied, so the
	// stopped candidate stays and the previous container is untouched.
	// The stop, start, and remove functions stay unset and panic if the
	// deployment continues past the uncertain delete.
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP}}
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{Workload: "storefront-jobs", Address: "old:8080", ContainerID: "old"}, true, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			return unconfirmed("delete route", errors.New("EOF"))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "delete deployed route") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeployBackgroundTransitionDeleteRejectionDisposesCandidate(t *testing.T) {
	// A definitive route-delete rejection proves the old route is still
	// active: the previous container stays untouched and the stopped
	// candidate is safely disposed. The stop function stays unset and
	// panics if the previous container is touched.
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP}}
	var removed []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{Workload: "storefront-jobs", Address: "old:8080", ContainerID: "old"}, true, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			return errors.New("route store corrupted")
		},
		removeFunc: func(_ context.Context, container Container) error {
			removed = append(removed, container.ID)

			return nil
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if err == nil || !strings.Contains(err.Error(), "delete deployed route") || EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, removed, []string{"new"})
}

func TestDeployBackgroundTransitionRestoreUncertaintyHalts(t *testing.T) {
	// The rollback restarts the previous container, but the route restore
	// answer is lost: the final state is unconfirmed and nothing else is
	// attempted.
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindHTTP}}
	var events []string
	runtime := &fakeRuntime{
		pullFunc: func(context.Context, string) (Image, error) {
			return Image{ID: "sha256:image-v2"}, nil
		},
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		matchesFunc: func(context.Context, Container, ContainerSpec) (bool, error) {
			return false, nil
		},
		createFunc: func(context.Context, ContainerSpec) (Container, error) {
			return Container{ID: "new"}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{Workload: "storefront-jobs", Address: "old:8080", ContainerID: "old"}, true, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			return nil
		},
		stopFunc: func(context.Context, Container) error {
			return nil
		},
		startFunc: func(context.Context, Container) error {
			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			return errors.New("unhealthy")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
		updateRouteFunc: func(context.Context, Route) error {
			events = append(events, "route-restore")

			return unconfirmed("update route", errors.New("EOF"))
		},
	}

	vm, _ := newHost(t, runtime)
	_, err := vm.Deploy(t.Context(), backgroundDefinition())
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "remote effects unconfirmed") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, []string{"remove-new", "route-restore"})
}

// newHost constructs a host for tests that records emitted progress facts.
func newHost(t *testing.T, runtime Runtime) (*Host, *[]Progress) {
	t.Helper()

	var facts []Progress
	vm, err := NewHost(runtime, func(fact Progress) { facts = append(facts, fact) }, testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	return vm, &facts
}

// silentReporter discards progress facts for tests that do not assert them.
func silentReporter() Reporter {
	return func(Progress) {}
}

func assertFacts(t *testing.T, got *[]Progress, want []Progress) {
	t.Helper()
	if got == nil || len(*got) != len(want) {
		t.Fatalf("facts = %+v, want %+v", got, want)
	}
	for index, fact := range *got {
		if fact != want[index] {
			t.Fatalf("facts = %+v, want %+v", *got, want)
		}
	}
}

// fakeRuntime records lifecycle calls through injected functions.
type fakeRuntime struct {
	closeFunc              func() error
	pullFunc               func(context.Context, string) (Image, error)
	workloadContainersFunc func(context.Context, string) ([]Container, error)
	matchesFunc            func(context.Context, Container, ContainerSpec) (bool, error)
	createFunc             func(context.Context, ContainerSpec) (Container, error)
	restartReplacementFunc func(context.Context, Container) (Container, error)
	startFunc              func(context.Context, Container) error
	backendAddressFunc     func(context.Context, Container, uint16) (string, error)
	routeFunc              func(context.Context, string) (Route, bool, error)
	updateRouteFunc        func(context.Context, Route) error
	deleteRouteFunc        func(context.Context, string, bool) error
	stopFunc               func(context.Context, Container) error
	verifyFunc             func(context.Context, Container) error
	logsFunc               func(context.Context, Container) (string, error)
	removeFunc             func(context.Context, Container) error
}

func (f *fakeRuntime) Close() error {
	if f.closeFunc == nil {
		return nil
	}
	return f.closeFunc()
}

func (f *fakeRuntime) Pull(ctx context.Context, reference string) (Image, error) {
	if f.pullFunc == nil {
		panic("fakeRuntime.pullFunc: not set")
	}
	return f.pullFunc(ctx, reference)
}

func (f *fakeRuntime) WorkloadContainers(ctx context.Context, workload string) ([]Container, error) {
	if f.workloadContainersFunc == nil {
		panic("fakeRuntime.workloadContainersFunc: not set")
	}
	return f.workloadContainersFunc(ctx, workload)
}

func (f *fakeRuntime) Matches(ctx context.Context, container Container, spec ContainerSpec) (bool, error) {
	if f.matchesFunc == nil {
		panic("fakeRuntime.matchesFunc: not set")
	}
	return f.matchesFunc(ctx, container, spec)
}

func (f *fakeRuntime) Create(ctx context.Context, spec ContainerSpec) (Container, error) {
	if f.createFunc == nil {
		panic("fakeRuntime.createFunc: not set")
	}
	return f.createFunc(ctx, spec)
}

func (f *fakeRuntime) RestartReplacement(ctx context.Context, current Container) (Container, error) {
	if f.restartReplacementFunc == nil {
		panic("fakeRuntime.restartReplacementFunc: not set")
	}
	return f.restartReplacementFunc(ctx, current)
}

func (f *fakeRuntime) Start(ctx context.Context, container Container) error {
	if f.startFunc == nil {
		panic("fakeRuntime.startFunc: not set")
	}
	return f.startFunc(ctx, container)
}

func (f *fakeRuntime) BackendAddress(ctx context.Context, container Container, port uint16) (string, error) {
	if f.backendAddressFunc == nil {
		panic("fakeRuntime.backendAddressFunc: not set")
	}
	return f.backendAddressFunc(ctx, container, port)
}

func (f *fakeRuntime) Route(ctx context.Context, workload string) (Route, bool, error) {
	if f.routeFunc == nil {
		panic("fakeRuntime.routeFunc: not set")
	}
	return f.routeFunc(ctx, workload)
}

func (f *fakeRuntime) UpdateRoute(ctx context.Context, route Route) error {
	if f.updateRouteFunc == nil {
		panic("fakeRuntime.updateRouteFunc: not set")
	}
	return f.updateRouteFunc(ctx, route)
}

func (f *fakeRuntime) DeleteRoute(ctx context.Context, workload string, allowMissing bool) error {
	if f.deleteRouteFunc == nil {
		panic("fakeRuntime.deleteRouteFunc: not set")
	}
	return f.deleteRouteFunc(ctx, workload, allowMissing)
}

func (f *fakeRuntime) Stop(ctx context.Context, container Container) error {
	if f.stopFunc == nil {
		panic("fakeRuntime.stopFunc: not set")
	}
	return f.stopFunc(ctx, container)
}

func (f *fakeRuntime) Verify(ctx context.Context, container Container) error {
	if f.verifyFunc == nil {
		panic("fakeRuntime.verifyFunc: not set")
	}
	return f.verifyFunc(ctx, container)
}

func (f *fakeRuntime) Logs(ctx context.Context, container Container) (string, error) {
	if f.logsFunc == nil {
		panic("fakeRuntime.logsFunc: not set")
	}
	return f.logsFunc(ctx, container)
}

func (f *fakeRuntime) Remove(ctx context.Context, container Container) error {
	if f.removeFunc == nil {
		panic("fakeRuntime.removeFunc: not set")
	}
	return f.removeFunc(ctx, container)
}

func backgroundDefinition() workload.Definition {
	return workload.Definition{
		Name: "storefront-jobs",
		GCP: workload.GCP{
			Project:   "acme-prod",
			Instances: []workload.Instance{{Zone: "us-central1-a", Name: "jobs-1"}},
		},
		Container: workload.ContainerConfig{
			Image:       "docker.io/example/jobs:v1",
			Command:     []string{"bin/jobs"},
			Environment: map[string]string{"APP_ENV": "production"},
		},
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

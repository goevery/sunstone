package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestRestartMissingWorkloadFails(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) { return nil, nil },
	}
	vm, facts := newHost(t, runtime)

	_, err := vm.Restart(t.Context(), "jobs")
	if err == nil || !strings.Contains(err.Error(), "workload is missing") {
		t.Fatalf("error = %v", err)
	}
	assertFacts(t, facts, nil)
}

func TestRestartRefusesAmbiguousStateWithoutMutating(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "a", Metadata: backgroundMetadata()}, {ID: "b", Metadata: backgroundMetadata()}}, nil
		},
		stopFunc:  func(context.Context, Container) error { events = append(events, "stop"); return nil },
		startFunc: func(context.Context, Container) error { events = append(events, "start"); return nil },
	}
	vm, facts := newHost(t, runtime)

	_, err := vm.Restart(t.Context(), "jobs")
	if err == nil || !strings.Contains(err.Error(), "refusing ambiguous state") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, nil)
	assertFacts(t, facts, nil)
}

func TestRestartRefusesInvalidDeployedMetadataWithoutMutating(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "legacy", Running: true}}, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop")

			return nil
		},
	}
	vm, facts := newHost(t, runtime)

	_, err := vm.Restart(t.Context(), "jobs")
	if err == nil || !strings.Contains(err.Error(), "invalid deployed metadata") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, nil)
	assertFacts(t, facts, nil)
}

func TestRestartRunningBackgroundContainerStopsStartsAndVerifies(t *testing.T) {
	current := Container{ID: "current", Running: true, Metadata: backgroundMetadata()}
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop")

			return nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start")

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			events = append(events, "verify")

			return nil
		},
	}
	vm, facts := newHost(t, runtime)

	outcome, err := vm.Restart(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Container != current.ID || !outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"stop", "start", "verify"})
	assertFacts(t, facts, []Progress{{Stage: StageRestarted}})
}

func TestRestartStoppedBackgroundContainerSkipsStop(t *testing.T) {
	current := Container{ID: "current", Metadata: backgroundMetadata()}
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop")

			return nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start")

			return nil
		},
		verifyFunc: func(context.Context, Container) error {
			events = append(events, "verify")

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	outcome, err := vm.Restart(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Container != current.ID || !outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"start", "verify"})
}

func TestRestartDeployedHTTPWorkloadSwitchesRouteAndRemovesPrevious(t *testing.T) {
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: httpMetadata()}
	replacement := Container{ID: "new", Name: "storefront-01234567"}
	var events []string
	var routed Route
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		restartReplacementFunc: func(_ context.Context, candidate Container) (Container, error) {
			if candidate.ID != current.ID {
				t.Fatalf("replacement built from %+v", candidate)
			}
			events = append(events, "clone")

			return replacement, nil
		},
		startFunc: func(context.Context, Container) error {
			events = append(events, "start-new")

			return nil
		},
		backendAddressFunc: func(_ context.Context, container Container, port uint16) (string, error) {
			if container.ID != replacement.ID || port != httpMetadata().HTTPPort {
				t.Fatalf("backend address for %+v port %d", container, port)
			}
			events = append(events, "backend")

			return "storefront-01234567:8080", nil
		},
		updateRouteFunc: func(_ context.Context, route Route) error {
			events = append(events, "route")
			routed = route

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, facts := newHost(t, runtime)

	outcome, err := vm.Restart(t.Context(), "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Container != replacement.ID || !outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"clone", "start-new", "backend", "route", "remove-old"})
	wantRoute := Route{Workload: "storefront", Address: "storefront-01234567:8080", ContainerID: replacement.ID, StartupProbePath: httpMetadata().StartupProbePath}
	if routed != wantRoute {
		t.Fatalf("route = %+v, want %+v", routed, wantRoute)
	}
	assertFacts(t, facts, []Progress{{Stage: StageRestarted}})
}

func TestRestartHTTPReplacementFailureCleansUpAndKeepsPrevious(t *testing.T) {
	current := Container{ID: "old", Running: true, Healthy: true, Metadata: httpMetadata()}
	replacement := Container{ID: "new"}
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		restartReplacementFunc: func(context.Context, Container) (Container, error) {
			return replacement, nil
		},
		startFunc: func(context.Context, Container) error {
			return errors.New("start failed")
		},
		logsFunc: func(context.Context, Container) (string, error) { return "crash", nil },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	_, err := vm.Restart(t.Context(), "storefront")
	if err == nil || !strings.Contains(err.Error(), "start replacement: start failed") || !strings.Contains(err.Error(), "replacement logs:\ncrash") {
		t.Fatalf("error = %v", err)
	}
	// Only the failed replacement is cleaned up; the previous container remains.
	assertStrings(t, events, []string{"remove-new"})
}

func TestRestartMarksStateUnconfirmedWhenVerificationIsLost(t *testing.T) {
	// The container was stopped and started with confirmed answers, but the
	// verification read lost the transport: the final state is unconfirmed.
	current := Container{ID: "container-1", Running: true, Metadata: backgroundMetadata()}
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{current}, nil
		},
		stopFunc:  func(context.Context, Container) error { return nil },
		startFunc: func(context.Context, Container) error { return nil },
		verifyFunc: func(context.Context, Container) error {
			return fmt.Errorf("inspect container: %w", io.EOF)
		},
	}
	vm, _ := newHost(t, runtime)

	_, err := vm.Restart(t.Context(), "storefront")
	if !EffectsUnconfirmed(err) || !strings.Contains(err.Error(), "verify restarted container") {
		t.Fatalf("error = %v", err)
	}
}

package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRemoveAbsentWorkloadIsNoOp(t *testing.T) {
	// No route, pull, create, start, verify, logs, or stop functions are set:
	// any forbidden call panics, and an empty candidate set must not contact
	// Sunbeam.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) { return nil, nil },
	}
	vm, facts := newHost(t, runtime)

	outcome, err := vm.Remove(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertFacts(t, facts, []Progress{{Stage: StageRemoved}})
}

func TestRemoveStopsRunningBackgroundContainersGracefully(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{
				{ID: "running", Running: true, Metadata: backgroundMetadata()},
				{ID: "stopped", Metadata: backgroundMetadata()},
			}, nil
		},
		stopFunc: func(_ context.Context, container Container) error {
			events = append(events, "stop-"+container.ID)

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, facts := newHost(t, runtime)

	outcome, err := vm.Remove(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	assertStrings(t, events, []string{"stop-running", "remove-running", "remove-stopped"})
	assertFacts(t, facts, []Progress{{Stage: StageRemoved}})
}

func TestRemoveHTTPDrainsRouteBeforeRemovingContainers(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "web", Running: true, Healthy: true, Metadata: httpMetadata()}}, nil
		},
		deleteRouteFunc: func(_ context.Context, workloadName string, allowMissing bool) error {
			if workloadName != "storefront" || !allowMissing {
				t.Fatalf("delete route = %q, %t", workloadName, allowMissing)
			}
			events = append(events, "delete-route")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	outcome, err := vm.Remove(t.Context(), "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Changed {
		t.Fatalf("outcome = %+v", outcome)
	}
	// Drained HTTP candidates are removed without being stopped.
	assertStrings(t, events, []string{"delete-route", "remove-web"})
}

func TestRemoveMixedContainersDrainsRouteAndStopsBackground(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{
				{ID: "web", Running: true, Metadata: httpMetadata()},
				{ID: "worker", Running: true, Metadata: backgroundMetadata()},
			}, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			events = append(events, "delete-route")

			return nil
		},
		stopFunc: func(_ context.Context, container Container) error {
			events = append(events, "stop-"+container.ID)

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	if _, err := vm.Remove(t.Context(), "storefront"); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, events, []string{"delete-route", "remove-web", "stop-worker", "remove-worker"})
}

func TestRemoveRefusesInvalidMetadataWithoutMutating(t *testing.T) {
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{
				{ID: "legacy", Running: true},
				{ID: "web", Running: true, Metadata: httpMetadata()},
			}, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error {
			events = append(events, "delete-route")

			return nil
		},
		stopFunc: func(context.Context, Container) error {
			events = append(events, "stop")

			return nil
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, facts := newHost(t, runtime)

	_, err := vm.Remove(t.Context(), "jobs")
	if err == nil || !strings.Contains(err.Error(), "invalid deployed metadata") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, nil)
	assertFacts(t, facts, nil)
}

func TestRemoveRouteFailurePreventsContainerRemoval(t *testing.T) {
	failure := errors.New("sunbeam unavailable")
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "web", Running: true, Metadata: httpMetadata()}}, nil
		},
		deleteRouteFunc: func(context.Context, string, bool) error { return failure },
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	_, err := vm.Remove(t.Context(), "storefront")
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "delete route") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, nil)
}

func TestRemoveHaltsAtFirstMutationFailure(t *testing.T) {
	failure := errors.New("stop failed")
	var events []string
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{
				{ID: "first", Running: true, Metadata: backgroundMetadata()},
				{ID: "second", Metadata: backgroundMetadata()},
			}, nil
		},
		stopFunc: func(_ context.Context, container Container) error {
			events = append(events, "stop-"+container.ID)

			return failure
		},
		removeFunc: func(_ context.Context, container Container) error {
			events = append(events, "remove-"+container.ID)

			return nil
		},
	}
	vm, _ := newHost(t, runtime)

	_, err := vm.Remove(t.Context(), "jobs")
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "stop container first") {
		t.Fatalf("error = %v", err)
	}
	assertStrings(t, events, []string{"stop-first"})
}

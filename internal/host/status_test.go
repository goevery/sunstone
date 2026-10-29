package host

import (
	"context"
	"errors"
	"testing"
)

func backgroundMetadata() *DeployedMetadata {
	return &DeployedMetadata{Kind: KindBackground}
}

func httpMetadata() *DeployedMetadata {
	return &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080, StartupProbePath: "/readyz"}
}

func TestStatusReportsMissingWorkload(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) { return nil, nil },
	}
	// No route function is set: a missing workload must not touch Sunbeam.
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateMissing || len(observation.Containers) != 0 || observation.Route != nil {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestStatusReportsAmbiguousWorkloadWithoutChoosing(t *testing.T) {
	// No route function is set: a fully background ambiguity must not touch
	// Sunbeam.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "a", Metadata: backgroundMetadata()}, {ID: "b", Metadata: backgroundMetadata()}}, nil
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateAmbiguous || len(observation.Containers) != 2 {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestAmbiguousUnclassifiableWorkloadDoesNotTouchSunbeam(t *testing.T) {
	// Interrupted replacements can leave containers without valid metadata;
	// an entirely unclassifiable set must not query the route.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "a"}, {ID: "b", Metadata: backgroundMetadata()}}, nil
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateAmbiguous || observation.Route != nil {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestAmbiguousHTTPWorkloadReportsActiveRouteWithoutChoosing(t *testing.T) {
	// The route is keyed by workload: an interrupted HTTP replacement's
	// active route is observable even though no container is chosen.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "previous", Running: true, Healthy: true, Metadata: httpMetadata()}, {ID: "replacement", Metadata: httpMetadata()}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{ContainerID: "previous", Address: "web-01234567:8080"}, true, nil
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateAmbiguous || len(observation.Containers) != 2 {
		t.Fatalf("observation = %+v", observation)
	}
	if observation.Route == nil || observation.Route.ContainerID != "previous" {
		t.Fatalf("route = %+v", observation.Route)
	}
}

func TestAmbiguousHTTPRouteFailurePreservesObservation(t *testing.T) {
	failure := errors.New("sunbeam unavailable")
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "a", Running: true, Healthy: true, Metadata: httpMetadata()}, {ID: "b", Metadata: httpMetadata()}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, failure
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "storefront")
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if observation.State != StateAmbiguous || len(observation.Containers) != 2 {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestStatusReportsInvalidDeployedMetadata(t *testing.T) {
	// Legacy or externally mutated containers without valid metadata must be
	// reported explicitly, never silently classified as background.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "legacy", Running: true, Healthy: true}}, nil
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateInvalidMetadata || len(observation.Containers) != 1 {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestStatusClassifiesBackgroundWithoutSunbeam(t *testing.T) {
	tests := []struct {
		name      string
		container Container
		want      string
	}{
		{name: "stopped", container: Container{ID: "c", Metadata: backgroundMetadata()}, want: StateStopped},
		{name: "unhealthy", container: Container{ID: "c", Running: true, Metadata: backgroundMetadata()}, want: StateUnhealthy},
		{name: "current", container: Container{ID: "c", Running: true, Healthy: true, Metadata: backgroundMetadata()}, want: StateCurrent},
	}
	for _, test := range tests {
		runtime := &fakeRuntime{
			workloadContainersFunc: func(context.Context, string) ([]Container, error) {
				return []Container{test.container}, nil
			},
		}
		vm, _ := newHost(t, runtime)

		observation, err := vm.Status(t.Context(), "jobs")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if observation.State != test.want {
			t.Fatalf("%s: state = %q, want %q", test.name, observation.State, test.want)
		}
	}
}

func TestStatusClassifiesDeployedHTTPWorkload(t *testing.T) {
	container := Container{ID: "c", Name: "web", Running: true, Healthy: true, Metadata: httpMetadata()}
	tests := []struct {
		name  string
		route func(context.Context, string) (Route, bool, error)
		want  string
	}{
		{
			name: "current",
			route: func(context.Context, string) (Route, bool, error) {
				return Route{ContainerID: "c", Address: "web:8080"}, true, nil
			},
			want: StateCurrent,
		},
		{
			name: "route missing",
			route: func(context.Context, string) (Route, bool, error) {
				return Route{}, false, nil
			},
			want: StateRouteMissing,
		},
		{
			name: "route mismatch",
			route: func(context.Context, string) (Route, bool, error) {
				return Route{ContainerID: "other", Address: "web:8080"}, true, nil
			},
			want: StateRouteMismatch,
		},
	}
	for _, test := range tests {
		runtime := &fakeRuntime{
			workloadContainersFunc: func(context.Context, string) ([]Container, error) {
				return []Container{container}, nil
			},
			routeFunc: test.route,
		}
		vm, _ := newHost(t, runtime)

		observation, err := vm.Status(t.Context(), "storefront")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if observation.State != test.want {
			t.Fatalf("%s: state = %q, want %q", test.name, observation.State, test.want)
		}
	}
}

func TestStatusObservesRouteForStoppedHTTPWorkload(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "c", Metadata: httpMetadata()}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{ContainerID: "c", Address: "web:8080"}, true, nil
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "storefront")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateStopped || observation.Route == nil || observation.Route.ContainerID != "c" {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestStatusReturnsErrorsWithPartialObservation(t *testing.T) {
	failure := errors.New("route unavailable")
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "c", Running: true, Healthy: true, Metadata: httpMetadata()}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, failure
		},
	}
	vm, _ := newHost(t, runtime)

	observation, err := vm.Status(t.Context(), "storefront")
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if len(observation.Containers) != 1 {
		t.Fatalf("partial observation lost containers: %+v", observation)
	}

	listFailure := errors.New("docker unavailable")
	runtime = &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, listFailure
		},
	}
	vm, _ = newHost(t, runtime)
	if _, err := vm.Status(t.Context(), "storefront"); !errors.Is(err, listFailure) {
		t.Fatalf("error = %v", err)
	}
}

func TestStatusNeverPullsCreatesOrMutates(t *testing.T) {
	// Any call to Pull, Create, Start, Stop, Verify, Logs, Remove,
	// UpdateRoute, or DeleteRoute panics in fakeRuntime when its function is
	// unset, so this test proves Status is observation-only.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "c", Running: true, Healthy: true, Metadata: backgroundMetadata()}}, nil
		},
	}
	vm, _ := newHost(t, runtime)

	if _, err := vm.Status(t.Context(), "jobs"); err != nil {
		t.Fatal(err)
	}
}

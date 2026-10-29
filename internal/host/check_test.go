package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckProvesDockerAccessWithoutMutating(t *testing.T) {
	listed := []Container{{ID: "container-1", Running: true, Healthy: true, Metadata: &DeployedMetadata{Kind: KindBackground}}}
	runtime := &fakeRuntime{
		// Pull, create, start, and route functions stay unset: Check is
		// observation only and panics if it touches any of them.
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return listed, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := vm.Check(t.Context(), "jobs", false); err != nil {
		t.Fatalf("background check = %v", err)
	}
}

func TestCheckRequiresNoSunbeamForBackgroundOrAbsentWorkloads(t *testing.T) {
	tests := []struct {
		name       string
		containers []Container
	}{
		{name: "zero containers", containers: nil},
		{name: "background containers", containers: []Container{
			{ID: "one", Metadata: &DeployedMetadata{Kind: KindBackground}},
			{ID: "two", Metadata: &DeployedMetadata{Kind: KindBackground}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// routeFunc stays unset: contacting Sunbeam fails the test.
			runtime := &fakeRuntime{
				workloadContainersFunc: func(context.Context, string) ([]Container, error) {
					return test.containers, nil
				},
			}
			vm, err := NewHost(runtime, silentReporter(), testHostConfig)
			if err != nil {
				t.Fatal(err)
			}

			if err := vm.Check(t.Context(), "jobs", false); err != nil {
				t.Fatalf("check = %v", err)
			}
		})
	}
}

func TestCheckContactsSunbeamWhenDesiredConfigurationIsHTTP(t *testing.T) {
	routed := false
	runtime := &fakeRuntime{
		// Zero deployed containers: the desired HTTP configuration alone
		// requires routing.
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			routed = true

			return Route{}, false, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := vm.Check(t.Context(), "storefront", true); err != nil {
		t.Fatalf("check = %v", err)
	}
	if !routed {
		t.Fatal("desired HTTP check did not verify Sunbeam access")
	}
}

func TestCheckRejectsUnclassifiableContainerWithoutDesiredHTTP(t *testing.T) {
	// A managed container without valid metadata may be a serving HTTP
	// container behind a live route: every operation that cannot repair it
	// is refused here, before any target of the batch is mutated. The route
	// function stays unset and panics if Check contacts Sunbeam.
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "legacy", Running: true, Healthy: true}}, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	err = vm.Check(t.Context(), "storefront", false)
	if err == nil || !strings.Contains(err.Error(), "container legacy has missing or invalid deployed metadata") {
		t.Fatalf("check = %v", err)
	}
}

func TestCheckPermitsMissingMetadataForDesiredHTTPRepair(t *testing.T) {
	// A desired HTTP deployment repairs missing metadata explicitly by
	// replacing the route, so Check still proves routing access and
	// permits the deploy.
	routed := false
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "legacy", Running: true, Healthy: true}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			routed = true

			return Route{}, false, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := vm.Check(t.Context(), "storefront", true); err != nil {
		t.Fatalf("check = %v", err)
	}
	if !routed {
		t.Fatal("desired HTTP check did not verify Sunbeam access")
	}
}

func TestCheckContactsSunbeamForDeployedHTTPEvenWithoutDesiredHTTP(t *testing.T) {
	// Name-only operations derive the routing requirement from deployed
	// metadata, never from configuration.
	routed := false
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return []Container{{ID: "web", Metadata: &DeployedMetadata{Kind: KindHTTP, HTTPPort: 8080}}}, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			routed = true

			return Route{ContainerID: "web"}, true, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := vm.Check(t.Context(), "storefront", false); err != nil {
		t.Fatalf("check = %v", err)
	}
	if !routed {
		t.Fatal("deployed HTTP metadata did not require Sunbeam access")
	}
}

func TestCheckMissingRouteProvesReachability(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, nil
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := vm.Check(t.Context(), "storefront", true); err != nil {
		t.Fatalf("missing route treated as failure: %v", err)
	}
}

func TestCheckReportsDockerAccessFailures(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, errors.New("docker daemon unreachable")
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	err = vm.Check(t.Context(), "jobs", false)
	if err == nil || !strings.Contains(err.Error(), "check Docker access") || !strings.Contains(err.Error(), "docker daemon unreachable") {
		t.Fatalf("check = %v", err)
	}
}

func TestCheckReportsSunbeamAccessFailures(t *testing.T) {
	runtime := &fakeRuntime{
		workloadContainersFunc: func(context.Context, string) ([]Container, error) {
			return nil, nil
		},
		routeFunc: func(context.Context, string) (Route, bool, error) {
			return Route{}, false, errors.New("sunbeam control refused")
		},
	}
	vm, err := NewHost(runtime, silentReporter(), testHostConfig)
	if err != nil {
		t.Fatal(err)
	}

	err = vm.Check(t.Context(), "storefront", true)
	if err == nil || !strings.Contains(err.Error(), "check Sunbeam access") || !strings.Contains(err.Error(), "sunbeam control refused") {
		t.Fatalf("check = %v", err)
	}
}

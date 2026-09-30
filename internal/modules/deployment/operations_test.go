package deployment

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

func TestStatusReportsAllTargetsAndRetainsConnectionFailures(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	workload.GCP.Instances = []deploy.Instance{{Zone: "a", Name: "web-1"}, {Zone: "b", Name: "web-2"}}
	container := deploy.Container{ID: "container-1", Name: "web", Image: "sha256:image", Running: true, Healthy: true}
	host := &mockHost{
		CloseFunc:              func() error { return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{container}, nil },
		RouteFunc: func(context.Context, string) (deploy.Route, bool, error) {
			return deploy.Route{ContainerID: container.ID, Address: "web:8080"}, true, nil
		},
	}
	loader := operationLoader(workload)
	connector := &mockConnector{ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
		if target.Instance == "web-1" {
			return host, nil
		}
		return nil, errors.New("IAP unavailable")
	}}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Status(context.Background(), WorkloadRequest{Filenames: []string{"workloads"}, ImpersonateServiceAccount: "operator@example.com"})
	if err == nil || !strings.Contains(err.Error(), "IAP unavailable") {
		t.Fatalf("status error = %v", err)
	}
	instances := result.Workloads[0].Instances
	if len(instances) != 2 || instances[0].State != "current" || instances[0].Containers[0].Image != "sha256:image" {
		t.Fatalf("first status = %+v", instances)
	}
	if instances[1].State != "unreachable" || !strings.Contains(instances[1].Error, "IAP unavailable") {
		t.Fatalf("second status = %+v", instances[1])
	}
}

func TestRestartBackgroundWorkloadStopsStartsAndVerifiesExistingContainer(t *testing.T) {
	workload := backgroundWorkload()
	current := deploy.Container{ID: "container-1", Running: true, Healthy: true}
	var events []string
	host := &mockHost{
		CloseFunc:              func() error { events = append(events, "close"); return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{current}, nil },
		StopFunc:               func(context.Context, deploy.Container) error { events = append(events, "stop"); return nil },
		StartFunc:              func(context.Context, deploy.Container) error { events = append(events, "start"); return nil },
		VerifyFunc:             func(context.Context, deploy.Container) error { events = append(events, "verify"); return nil },
	}
	module := operationModule(workload, host)

	result, err := module.Restart(context.Background(), WorkloadRequest{Filenames: []string{"workload.yaml"}, ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Workloads) != 1 || result.Workloads[0].Instances[0].Container != current.ID {
		t.Fatalf("result = %+v", result)
	}
	assertStrings(t, events, []string{"stop", "start", "verify", "close"})
}

func TestRestartMissingWorkloadStopsBeforeNextVM(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{{Zone: "a", Name: "jobs-1"}, {Zone: "b", Name: "jobs-2"}}
	host := &mockHost{
		CloseFunc:              func() error { return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return nil, nil },
	}
	var connected []string
	connector := &mockConnector{ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
		connected = append(connected, target.Instance)
		return host, nil
	}}
	module := newModule(operationLoader(workload), connector, io.Discard)

	result, err := module.Restart(context.Background(), WorkloadRequest{Filenames: []string{"workload.yaml"}, ImpersonateServiceAccount: "operator@example.com"})
	if err == nil || !strings.Contains(err.Error(), "workload is missing") {
		t.Fatalf("error = %v", err)
	}
	if len(result.Workloads) != 1 || len(result.Workloads[0].Instances) != 0 {
		t.Fatalf("partial result = %+v", result)
	}
	assertStrings(t, connected, []string{"jobs-1"})
}

func TestRestartHTTPWorkloadSwitchesToReplacementBeforeRemovingCurrent(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	current := deploy.Container{ID: "old", Running: true, Healthy: true}
	replacement := deploy.Container{ID: "new", Name: "web-new"}
	var events []string
	host := &mockHost{
		CloseFunc:              func() error { return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{current}, nil },
		PullFunc:               func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			events = append(events, "create")
			return replacement, nil
		},
		StartFunc:          func(context.Context, deploy.Container) error { events = append(events, "start"); return nil },
		BackendAddressFunc: func(context.Context, deploy.Container, uint16) (string, error) { return "web-new:8080", nil },
		UpdateRouteFunc:    func(context.Context, deploy.Route) error { events = append(events, "route"); return nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := operationModule(workload, host)

	result, err := module.Restart(context.Background(), WorkloadRequest{Filenames: []string{"workload.yaml"}, ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Workloads[0].Instances[0].Container != replacement.ID {
		t.Fatalf("result = %+v", result)
	}
	assertStrings(t, events, []string{"create", "start", "route", "remove-old"})
}

func TestRemoveBackgroundWorkloadStopsBeforeRemovingContainer(t *testing.T) {
	workload := backgroundWorkload()
	current := deploy.Container{ID: "container-1", Running: true}
	var events []string
	host := &mockHost{
		CloseFunc:              func() error { return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{current}, nil },
		StopFunc:               func(context.Context, deploy.Container) error { events = append(events, "stop"); return nil },
		RemoveFunc:             func(context.Context, deploy.Container) error { events = append(events, "remove"); return nil },
	}
	module := operationModule(workload, host)

	if _, err := module.Remove(context.Background(), WorkloadRequest{Filenames: []string{"workload.yaml"}, ImpersonateServiceAccount: "operator@example.com"}); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, events, []string{"stop", "remove"})
}

func TestRemoveHTTPWorkloadDrainsRouteBeforeRemovingAllContainers(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	containers := []deploy.Container{{ID: "one", Running: true}, {ID: "two"}}
	var events []string
	host := &mockHost{
		CloseFunc:              func() error { events = append(events, "close"); return nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return containers, nil },
		DeleteRouteFunc: func(_ context.Context, workload string, allowMissing bool) error {
			if workload != "storefront-jobs" || !allowMissing {
				t.Fatalf("delete route = %q, %t", workload, allowMissing)
			}
			events = append(events, "delete-route")
			return nil
		},
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := operationModule(workload, host)

	result, err := module.Remove(context.Background(), WorkloadRequest{Filenames: []string{"workload.yaml"}, ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Workloads[0].Instances[0].Changed {
		t.Fatalf("result = %+v", result)
	}
	assertStrings(t, events, []string{"delete-route", "remove-one", "remove-two", "close"})
}

func operationLoader(workloads ...deploy.Workload) *mockLoader {
	return &mockLoader{
		LoadFunc:    func(string) (deploy.Workload, error) { return deploy.Workload{}, errors.New("unexpected Load") },
		LoadAllFunc: func([]string) ([]deploy.Workload, error) { return workloads, nil },
	}
}

func operationModule(workload deploy.Workload, host deploy.Host) Module {
	connector := &mockConnector{ConnectFunc: func(context.Context, deploy.Target, string) (deploy.Host, error) { return host, nil }}
	return newModule(operationLoader(workload), connector, io.Discard)
}

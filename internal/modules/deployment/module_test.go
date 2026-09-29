package deployment

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

func TestDeploysHTTPReplacementBeforeRemovingPreviousContainer(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	previous := deploy.Container{ID: "old", Running: true, Healthy: true}
	replacement := deploy.Container{ID: "new"}
	var events []string
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc:  func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{previous}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return false, nil },
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			events = append(events, "create")
			return replacement, nil
		},
		StartFunc: func(context.Context, deploy.Container) error {
			events = append(events, "start-new")
			return nil
		},
		BackendAddressFunc: func(context.Context, deploy.Container, uint16) (string, error) {
			return "storefront-0123456789ab-01234567:8080", nil
		},
		RouteFunc: func(context.Context, string) (deploy.Route, bool, error) {
			return deploy.Route{}, false, nil
		},
		UpdateRouteFunc: func(context.Context, deploy.Route) error {
			events = append(events, "route")
			return nil
		},
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := moduleWith(t, workload, host)

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 1 || !result.Instances[0].Changed {
		t.Fatalf("result = %+v", result)
	}
	assertStrings(t, events, []string{"create", "start-new", "route", "remove-old"})
}

func TestCurrentHTTPWorkloadAndRouteAreNotReplaced(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	current := deploy.Container{ID: "current", Running: true, Healthy: true}
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc:  func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{current}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return true, nil },
		BackendAddressFunc: func(context.Context, deploy.Container, uint16) (string, error) {
			return "storefront-0123456789ab-01234567:8080", nil
		},
		RouteFunc: func(context.Context, string) (deploy.Route, bool, error) {
			return deploy.Route{Workload: workload.Name, Address: "storefront-0123456789ab-01234567:8080", ContainerID: current.ID, StartupProbePath: "/readyz"}, true, nil
		},
	}
	module := moduleWith(t, workload, host)

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 1 || result.Instances[0].Changed {
		t.Fatalf("result = %+v", result)
	}
}

func TestFailedHTTPRouteUpdateDoesNotRestartStoppedPreviousContainer(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	previous := deploy.Container{ID: "old"}
	replacement := deploy.Container{ID: "new"}
	var events []string
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc:  func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{previous}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return false, nil },
		CreateFunc:  func(context.Context, deploy.ContainerSpec) (deploy.Container, error) { return replacement, nil },
		StartFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "start-"+container.ID)
			return nil
		},
		BackendAddressFunc: func(context.Context, deploy.Container, uint16) (string, error) {
			return "storefront-0123456789ab-01234567:8080", nil
		},
		UpdateRouteFunc: func(context.Context, deploy.Route) error { return errors.New("not ready") },
		LogsFunc:        func(context.Context, deploy.Container) (string, error) { return "booting", nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := moduleWith(t, workload, host)

	_, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err == nil || !strings.Contains(err.Error(), "update route replacement: not ready") || !strings.Contains(err.Error(), "booting") {
		t.Fatalf("expected route diagnostics, got %v", err)
	}
	assertStrings(t, events, []string{"start-new", "remove-new"})
}

func TestDeploysBackgroundWorkloadToVMsSequentially(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
	}
	var events []string
	hosts := map[string]deploy.Host{
		"jobs-1": successfulHost(&events, "jobs-1", "container-1"),
		"jobs-2": successfulHost(&events, "jobs-2", "container-2"),
	}
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, serviceAccount string) (deploy.Host, error) {
			if serviceAccount != "operator@example.com" {
				t.Errorf("impersonated service account = %q", serviceAccount)
			}
			events = append(events, "connect-"+target.Instance)
			return hosts[target.Instance], nil
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 2 {
		t.Fatalf("instance results = %+v", result.Instances)
	}
	if result.Instances[0].Zone != "us-central1-a" || result.Instances[0].Instance != "jobs-1" || result.Instances[0].Container != "container-1" || !result.Instances[0].Changed {
		t.Fatalf("first instance result = %+v", result.Instances[0])
	}
	if result.Instances[1].Zone != "us-central1-b" || result.Instances[1].Instance != "jobs-2" || result.Instances[1].Container != "container-2" || !result.Instances[1].Changed {
		t.Fatalf("second instance result = %+v", result.Instances[1])
	}
	assertStrings(t, events, []string{
		"connect-jobs-1", "pull-jobs-1", "start-jobs-1", "verify-jobs-1", "close-jobs-1",
		"connect-jobs-2", "pull-jobs-2", "start-jobs-2", "verify-jobs-2", "close-jobs-2",
	})
}

func TestConnectionCloseFailureStopsBeforeNextVM(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
	}
	host := &mockHost{
		CloseFunc: func() error { return errors.New("close failed") },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return nil, nil },
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			return deploy.Container{ID: "container-1"}, nil
		},
		StartFunc:  func(context.Context, deploy.Container) error { return nil },
		VerifyFunc: func(context.Context, deploy.Container) error { return nil },
	}
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	var connected []string
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
			connected = append(connected, target.Instance)
			return host, nil
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "deploy to jobs-1: close connection: close failed") {
		t.Fatalf("expected target close error, got %v", err)
	}
	if len(result.Instances) != 0 {
		t.Fatalf("partial result = %+v", result)
	}
	assertStrings(t, connected, []string{"jobs-1"})
}

func TestFailedVMReturnsCompletedResultsAndStopsRollout(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
		{Zone: "us-central1-c", Name: "jobs-3"},
	}
	var events []string
	first := successfulHost(&events, "jobs-1", "container-1")
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
			events = append(events, "connect-"+target.Instance)
			if target.Instance == "jobs-1" {
				return first, nil
			}
			return nil, errors.New("IAP unavailable")
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "deploy to jobs-2: connect: IAP unavailable") {
		t.Fatalf("expected target connection error, got %v", err)
	}
	if len(result.Instances) != 1 || result.Instances[0].Instance != "jobs-1" {
		t.Fatalf("partial result = %+v", result)
	}
	assertStrings(t, events, []string{
		"connect-jobs-1", "pull-jobs-1", "start-jobs-1", "verify-jobs-1", "close-jobs-1",
		"connect-jobs-2",
	})
}

func TestFailedReplacementRollsBackOnlyFailedVM(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
		{Zone: "us-central1-c", Name: "jobs-3"},
	}
	var events []string
	first := successfulHost(&events, "jobs-1", "container-1")
	previous := deploy.Container{ID: "old-2", Running: true, Healthy: true}
	replacement := deploy.Container{ID: "new-2"}
	second := &mockHost{
		CloseFunc: func() error {
			events = append(events, "close-jobs-2")
			return nil
		},
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{previous}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return false, nil },
		CreateFunc:  func(context.Context, deploy.ContainerSpec) (deploy.Container, error) { return replacement, nil },
		StopFunc: func(context.Context, deploy.Container) error {
			events = append(events, "stop-old-2")
			return nil
		},
		StartFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "start-"+container.ID)
			return nil
		},
		VerifyFunc: func(context.Context, deploy.Container) error { return errors.New("unhealthy") },
		LogsFunc:   func(context.Context, deploy.Container) (string, error) { return "", nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
			events = append(events, "connect-"+target.Instance)
			switch target.Instance {
			case "jobs-1":
				return first, nil
			case "jobs-2":
				return second, nil
			default:
				t.Fatalf("unexpected connection to %s", target.Instance)
				return nil, nil
			}
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "deploy to jobs-2: verify replacement: unhealthy") {
		t.Fatalf("expected target replacement error, got %v", err)
	}
	if len(result.Instances) != 1 || result.Instances[0].Instance != "jobs-1" {
		t.Fatalf("partial result = %+v", result)
	}
	assertStrings(t, events, []string{
		"connect-jobs-1", "pull-jobs-1", "start-jobs-1", "verify-jobs-1", "close-jobs-1",
		"connect-jobs-2", "stop-old-2", "start-new-2", "remove-new-2", "start-old-2", "close-jobs-2",
	})
}

func TestCancellationStopsBeforeConnectingToNextVM(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return nil, nil },
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			return deploy.Container{ID: "container-1"}, nil
		},
		StartFunc: func(context.Context, deploy.Container) error { return nil },
		VerifyFunc: func(context.Context, deploy.Container) error {
			cancel()
			return nil
		},
	}
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	var connected []string
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
			connected = append(connected, target.Instance)
			return first, nil
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(ctx, Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "deploy to jobs-2") {
		t.Fatalf("expected cancellation before second VM, got %v", err)
	}
	if len(result.Instances) != 1 || result.Instances[0].Instance != "jobs-1" {
		t.Fatalf("partial result = %+v", result)
	}
	assertStrings(t, connected, []string{"jobs-1"})
}

func TestDeploysBackgroundWorkload(t *testing.T) {
	var created deploy.ContainerSpec
	started := false
	verified := false
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return nil, nil
		},
		CreateFunc: func(_ context.Context, spec deploy.ContainerSpec) (deploy.Container, error) {
			created = spec
			return deploy.Container{ID: "new-container", Name: spec.Name}, nil
		},
		StartFunc: func(context.Context, deploy.Container) error {
			started = true
			return nil
		},
		VerifyFunc: func(context.Context, deploy.Container) error {
			verified = true
			return nil
		},
	}
	module := moduleWith(t, backgroundWorkload(), host)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Workload != "storefront-jobs" || len(result.Instances) != 1 || result.Instances[0].Instance != "jobs-1" || !result.Instances[0].Changed {
		t.Fatalf("unexpected result: %+v", result)
	}
	if created.Name != "storefront-jobs" || created.Image.ID != "sha256:image-v1" {
		t.Fatalf("unexpected container: %+v", created)
	}
	if !started || !verified {
		t.Fatalf("container was not started and verified: started=%t verified=%t", started, verified)
	}
}

func TestCurrentVMIsReportedAndRolloutContinues(t *testing.T) {
	workload := backgroundWorkload()
	workload.GCP.Instances = []deploy.Instance{
		{Zone: "us-central1-a", Name: "jobs-1"},
		{Zone: "us-central1-b", Name: "jobs-2"},
	}
	current := deploy.Container{ID: "current-1", Running: true, Healthy: true}
	first := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{current}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return true, nil },
	}
	var events []string
	second := successfulHost(&events, "jobs-2", "container-2")
	loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
			if target.Instance == "jobs-1" {
				return first, nil
			}
			return second, nil
		},
	}
	module := newModule(loader, connector, io.Discard)

	result, err := module.Deploy(context.Background(), Request{
		Filename:                  "workload.yaml",
		ImpersonateServiceAccount: "operator@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 2 || result.Instances[0].Changed || !result.Instances[1].Changed {
		t.Fatalf("instance results = %+v", result.Instances)
	}
	if result.Instances[0].Container != "current-1" || result.Instances[1].Container != "container-2" {
		t.Fatalf("instance results = %+v", result.Instances)
	}
}

func TestCurrentBackgroundWorkloadIsNotReplaced(t *testing.T) {
	current := deploy.Container{ID: "current", Name: "storefront-jobs", Running: true, Healthy: true}
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{current}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) {
			return true, nil
		},
	}
	module := moduleWith(t, backgroundWorkload(), host)

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 1 || result.Instances[0].Changed || result.Instances[0].Container != current.ID {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestChangedBackgroundWorkloadStopsBeforeStartingReplacement(t *testing.T) {
	current := deploy.Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true}
	replacement := deploy.Container{ID: "new", Name: "storefront-jobs"}
	var events []string
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{current}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) {
			return false, nil
		},
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			events = append(events, "create")
			return replacement, nil
		},
		StopFunc: func(context.Context, deploy.Container) error {
			events = append(events, "stop-old")
			return nil
		},
		StartFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "start-"+container.ID)
			return nil
		},
		VerifyFunc: func(context.Context, deploy.Container) error {
			events = append(events, "verify-new")
			return nil
		},
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := moduleWith(t, backgroundWorkload(), host)

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Instances) != 1 || !result.Instances[0].Changed {
		t.Fatal("changed deployment reported no change")
	}
	assertStrings(t, events, []string{"create", "stop-old", "start-new", "verify-new", "remove-old"})
}

func TestFailedReplacementRestartsPreviousRunningContainer(t *testing.T) {
	previous := deploy.Container{ID: "old", Name: "storefront-jobs-old", Running: true, Healthy: true}
	replacement := deploy.Container{ID: "new", Name: "storefront-jobs-new"}
	var events []string
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v2"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{previous}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) {
			return false, nil
		},
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			return replacement, nil
		},
		StopFunc: func(context.Context, deploy.Container) error {
			events = append(events, "stop-old")
			return nil
		},
		StartFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "start-"+container.ID)
			return nil
		},
		VerifyFunc: func(context.Context, deploy.Container) error {
			return errors.New("unhealthy")
		},
		LogsFunc: func(context.Context, deploy.Container) (string, error) { return "crash", nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := moduleWith(t, backgroundWorkload(), host)

	_, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err == nil || !strings.Contains(err.Error(), "replacement logs:\ncrash") {
		t.Fatalf("expected failed replacement diagnostics, got %v", err)
	}
	assertStrings(t, events, []string{"stop-old", "start-new", "remove-new", "start-old"})
}

func TestFailedReplacementDoesNotRestartPreviouslyCrashedContainer(t *testing.T) {
	crashed := deploy.Container{ID: "old", Name: "storefront-jobs-old"}
	replacement := deploy.Container{ID: "new", Name: "storefront-jobs"}
	var events []string
	host := &mockHost{
		CloseFunc: func() error { return nil },
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) {
			return []deploy.Container{crashed}, nil
		},
		MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) {
			return false, nil
		},
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			return replacement, nil
		},
		StopFunc: func(context.Context, deploy.Container) error {
			events = append(events, "stop-old")
			return nil
		},
		StartFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "start-"+container.ID)
			return errors.New("start failed")
		},
		LogsFunc: func(context.Context, deploy.Container) (string, error) { return "", nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			events = append(events, "remove-"+container.ID)
			return nil
		},
	}
	module := moduleWith(t, backgroundWorkload(), host)

	_, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err == nil {
		t.Fatal("expected failed replacement")
	}
	assertStrings(t, events, []string{"start-new", "remove-new"})
}

func moduleWith(t *testing.T, workload deploy.Workload, host deploy.Host) Module {
	t.Helper()
	loader := &mockLoader{
		LoadFunc: func(string) (deploy.Workload, error) { return workload, nil },
	}
	connector := &mockConnector{
		ConnectFunc: func(_ context.Context, _ deploy.Target, serviceAccount string) (deploy.Host, error) {
			if serviceAccount != "operator@example.com" {
				t.Errorf("impersonated service account = %q", serviceAccount)
			}
			return host, nil
		},
	}

	return newModule(loader, connector, io.Discard)
}

func successfulHost(events *[]string, instance, container string) deploy.Host {
	return &mockHost{
		CloseFunc: func() error {
			*events = append(*events, "close-"+instance)
			return nil
		},
		PullFunc: func(context.Context, string) (deploy.Image, error) {
			*events = append(*events, "pull-"+instance)
			return deploy.Image{ID: "sha256:image-v1"}, nil
		},
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return nil, nil },
		CreateFunc: func(context.Context, deploy.ContainerSpec) (deploy.Container, error) {
			return deploy.Container{ID: container}, nil
		},
		StartFunc: func(context.Context, deploy.Container) error {
			*events = append(*events, "start-"+instance)
			return nil
		},
		VerifyFunc: func(context.Context, deploy.Container) error {
			*events = append(*events, "verify-"+instance)
			return nil
		},
	}
}

func backgroundWorkload() deploy.Workload {
	return deploy.Workload{
		Name: "storefront-jobs",
		GCP: deploy.GCP{
			Project:   "acme-prod",
			Instances: []deploy.Instance{{Zone: "us-central1-a", Name: "jobs-1"}},
		},
		Container: deploy.ContainerConfig{
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

package deployment

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

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
		Filename:    "workload.yaml",
		OSLoginUser: "operator@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Workload != "storefront-jobs" || result.Instance != "jobs-1" || !result.Changed {
		t.Fatalf("unexpected result: %+v", result)
	}
	if created.Name != "storefront-jobs" || created.Image.ID != "sha256:image-v1" {
		t.Fatalf("unexpected container: %+v", created)
	}
	if !started || !verified {
		t.Fatalf("container was not started and verified: started=%t verified=%t", started, verified)
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

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", OSLoginUser: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Container != current.ID {
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

	result, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", OSLoginUser: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
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

	_, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", OSLoginUser: "operator@example.com"})
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

	_, err := module.Deploy(context.Background(), Request{Filename: "workload.yaml", OSLoginUser: "operator@example.com"})
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
		ConnectFunc: func(context.Context, deploy.Target, string) (deploy.Host, error) { return host, nil },
	}

	return newModule(loader, connector, io.Discard)
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

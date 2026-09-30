package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

func TestSecretStartFailurePreservesHTTPWorkload(t *testing.T) {
	workload := backgroundWorkload()
	workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	workload.Container.SecretEnvironment = map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"}
	previous := deploy.Container{ID: "previous", Running: true}
	candidate := deploy.Container{ID: "candidate"}
	removed := false
	host := &mockHost{
		CloseFunc:              func() error { return nil },
		PullFunc:               func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
		WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{previous}, nil },
		MatchesFunc:            func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) { return false, nil },
		CreateFunc: func(_ context.Context, spec deploy.ContainerSpec) (deploy.Container, error) {
			if spec.SecretEnvironment["API_KEY"] != "projects/p/secrets/key/versions/1" {
				t.Fatal("secret reference lost")
			}
			return candidate, nil
		},
		StartFunc: func(context.Context, deploy.Container) error { return errors.New("access secret: HTTP 403 Forbidden") },
		LogsFunc:  func(context.Context, deploy.Container) (string, error) { return "", nil },
		RemoveFunc: func(_ context.Context, container deploy.Container) error {
			if container.ID != candidate.ID {
				t.Fatal("previous workload removed")
			}
			removed = true
			return nil
		},
		StopFunc:        func(context.Context, deploy.Container) error { t.Fatal("previous workload stopped"); return nil },
		UpdateRouteFunc: func(context.Context, deploy.Route) error { t.Fatal("route changed"); return nil },
	}
	result, err := moduleWith(t, workload, host).Deploy(context.Background(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !removed || len(result.Instances) != 0 {
		t.Fatalf("result=%+v err=%v removed=%v", result, err, removed)
	}
}

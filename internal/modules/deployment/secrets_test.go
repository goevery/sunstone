package deployment

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

func TestSecretResolutionFailurePreservesCurrentWorkloadAndStopsRollout(t *testing.T) {
	for _, kind := range []string{"background", "HTTP"} {
		for _, phase := range []string{"compare", "create"} {
			t.Run(phase+"-"+kind, func(t *testing.T) {
				workload := backgroundWorkload()
				workload.GCP.Instances = []deploy.Instance{{Name: "first", Zone: "zone"}, {Name: "later", Zone: "zone"}}
				workload.Container.SecretEnvironment = map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"}
				if kind == "HTTP" {
					workload.HTTP = &deploy.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
				}
				failure := errors.New("access secret projects/p/secrets/key/versions/1: HTTP 403 Forbidden")
				previous := deploy.Container{ID: "previous", Running: true, Healthy: true}
				host := &mockHost{
					CloseFunc:              func() error { return nil },
					PullFunc:               func(context.Context, string) (deploy.Image, error) { return deploy.Image{ID: "image"}, nil },
					WorkloadContainersFunc: func(context.Context, string) ([]deploy.Container, error) { return []deploy.Container{previous}, nil },
					MatchesFunc: func(context.Context, deploy.Container, deploy.ContainerSpec) (bool, error) {
						if phase == "compare" {
							return false, failure
						}
						return false, nil
					},
					CreateFunc: func(_ context.Context, spec deploy.ContainerSpec) (deploy.Container, error) {
						if phase == "compare" {
							t.Fatal("created a candidate after failed comparison")
						}
						if spec.SecretEnvironment["API_KEY"] != "projects/p/secrets/key/versions/1" {
							t.Fatal("secret reference lost")
						}
						return deploy.Container{}, failure
					},
					StopFunc: func(context.Context, deploy.Container) error {
						t.Fatal("stopped previous container before resolving secrets")
						return nil
					},
					StartFunc: func(context.Context, deploy.Container) error {
						t.Fatal("started a container after failed resolution")
						return nil
					},
					RemoveFunc:      func(context.Context, deploy.Container) error { t.Fatal("removed previous container"); return nil },
					UpdateRouteFunc: func(context.Context, deploy.Route) error { t.Fatal("changed HTTP route"); return nil },
				}
				loader := &mockLoader{LoadFunc: func(string) (deploy.Workload, error) { return workload, nil }}
				connections := 0
				connector := &mockConnector{ConnectFunc: func(_ context.Context, target deploy.Target, _ string) (deploy.Host, error) {
					connections++
					if target.Instance != "first" {
						t.Fatal("continued rollout after secret failure")
					}
					return host, nil
				}}
				result, err := newModule(loader, connector, io.Discard).Deploy(t.Context(), Request{Filename: "workload.yaml", ImpersonateServiceAccount: "operator@example.com"})
				if !errors.Is(err, failure) || !strings.Contains(err.Error(), "HTTP 403") || len(result.Instances) != 0 || connections != 1 {
					t.Fatalf("result=%+v err=%v connections=%d", result, err, connections)
				}
			})
		}
	}
}

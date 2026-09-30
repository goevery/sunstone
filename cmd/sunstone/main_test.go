package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment"
)

type fakeModule struct {
	status  func(context.Context, deployment.WorkloadRequest) (deployment.StatusResult, error)
	restart func(context.Context, deployment.WorkloadRequest) (deployment.OperationResult, error)
	remove  func(context.Context, deployment.WorkloadRequest) (deployment.OperationResult, error)
}

func (*fakeModule) Deploy(context.Context, deployment.Request) (deployment.Result, error) {
	return deployment.Result{}, nil
}

func (module *fakeModule) Status(ctx context.Context, request deployment.WorkloadRequest) (deployment.StatusResult, error) {
	return module.status(ctx, request)
}

func (module *fakeModule) Restart(ctx context.Context, request deployment.WorkloadRequest) (deployment.OperationResult, error) {
	return module.restart(ctx, request)
}

func (module *fakeModule) Remove(ctx context.Context, request deployment.WorkloadRequest) (deployment.OperationResult, error) {
	return module.remove(ctx, request)
}

func TestStatusRendersObservationsBeforeReturningPartialFailure(t *testing.T) {
	failure := errors.New("one target unavailable")
	module := &fakeModule{status: func(_ context.Context, request deployment.WorkloadRequest) (deployment.StatusResult, error) {
		if strings.Join(request.Filenames, ",") != "one.yaml,workloads" || request.ImpersonateServiceAccount != "operator@example.com" {
			t.Fatalf("request = %+v", request)
		}
		return deployment.StatusResult{
			Workloads: []deployment.WorkloadStatus{
				{
					Workload: "storefront",
					Instances: []deployment.InstanceStatus{
						{
							Zone: "us-central1-a", Instance: "web-1", State: "current",
							Containers: []deployment.ContainerStatus{{ID: "container-1", Name: "web", Image: "sha256:image", Running: true, Healthy: true}},
							Route:      &deployment.RouteStatus{Address: "web:8080", ContainerID: "container-1"},
						},
					},
				},
			},
		}, failure
	}}
	var output bytes.Buffer
	command := commandWith(func(io.Writer) (deployment.Module, error) { return module, nil })
	command.Writer = &output

	err := command.Run(context.Background(), []string{"sunstone", "status", "-f", "one.yaml", "-f", "workloads", "--impersonate-service-account", "operator@example.com"})
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	for _, expected := range []string{"storefront / web-1 (us-central1-a): current", "container: container-1", "route: web:8080 -> container-1"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestLifecycleCommandsDispatchToModule(t *testing.T) {
	for _, name := range []string{"restart", "remove"} {
		t.Run(name, func(t *testing.T) {
			called := false
			operation := func(_ context.Context, request deployment.WorkloadRequest) (deployment.OperationResult, error) {
				called = request.ImpersonateServiceAccount == "operator@example.com" && len(request.Filenames) == 1
				return deployment.OperationResult{}, nil
			}
			module := &fakeModule{
				status: func(context.Context, deployment.WorkloadRequest) (deployment.StatusResult, error) {
					return deployment.StatusResult{}, nil
				},
				restart: operation,
				remove:  operation,
			}
			command := commandWith(func(io.Writer) (deployment.Module, error) { return module, nil })
			if err := command.Run(context.Background(), []string{"sunstone", name, "-f", "workload.yaml", "--impersonate-service-account", "operator@example.com"}); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatalf("%s was not dispatched", name)
			}
		})
	}
}

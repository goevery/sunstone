package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/workload"
)

func TestStatusRendersObservationsBeforeReturningPartialFailure(t *testing.T) {
	failure := errors.New("one target unavailable")
	var serviceAccounts []string
	observed := host.Observation{
		State:      host.StateCurrent,
		Containers: []host.Container{{ID: "container-1", Name: "web", Image: "sha256:image", Running: true, Healthy: true}},
		Route:      &host.Route{Address: "web:8080", ContainerID: "container-1"},
	}
	provider := &fakeProvider{open: func(_ context.Context, target deployment.Target, _ host.Reporter) (deployment.Handle, error) {
		if target.Instance == "web-1" {
			return deployment.Handle{
				Host:  &fakeHost{status: func(context.Context, string) (host.Observation, error) { return observed, nil }},
				Close: func() error { return nil },
			}, nil
		}

		return deployment.Handle{}, failure
	}}
	command := commandWith(func(serviceAccount string) (commandProvider, error) {
		serviceAccounts = append(serviceAccounts, serviceAccount)

		return provider, nil
	})
	directory := t.TempDir()
	var output bytes.Buffer
	command.Writer = &output

	err := command.Run(context.Background(), []string{
		"sunstone", "status",
		"-f", writeWorkload(t, directory, "storefront", "web-1"),
		"-f", writeWorkload(t, directory, "jobs", "web-2"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if len(serviceAccounts) != 1 || serviceAccounts[0] != "operator@example.com" {
		t.Fatalf("service accounts = %v", serviceAccounts)
	}
	// The provider is closed on every exit path, including partial failure.
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
	for _, expected := range []string{
		"storefront / web-1 (us-central1-a): current",
		"container: container-1 name=web image=sha256:image running=true healthy=true",
		"route: web:8080 -> container-1",
		"jobs / web-2 (us-central1-a): unreachable",
		"error: connect: one target unavailable",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestLifecycleCommandsDispatchToProvider(t *testing.T) {
	for _, name := range []string{"restart", "remove"} {
		t.Run(name, func(t *testing.T) {
			var operated []string
			targetHost := &fakeHost{
				restart: func(_ context.Context, workloadName string) (host.Result, error) {
					operated = append(operated, "restart:"+workloadName)

					return host.Result{Changed: true}, nil
				},
				remove: func(_ context.Context, workloadName string) (host.Result, error) {
					operated = append(operated, "remove:"+workloadName)

					return host.Result{Changed: true}, nil
				},
			}
			provider := providerOf(targetHost)
			command := commandWith(func(string) (commandProvider, error) {
				return provider, nil
			})
			command.Writer = io.Discard

			err := command.Run(context.Background(), []string{
				"sunstone", name,
				"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
				"--impersonate-service-account", "operator@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(operated) != 1 || operated[0] != name+":storefront" {
				t.Fatalf("operated = %v", operated)
			}
			if provider.closes != 1 {
				t.Fatalf("provider closes = %d", provider.closes)
			}
		})
	}
}

func TestDeployPrefersOperationFailureOverProgressFailure(t *testing.T) {
	operation := errors.New("connect failed")
	targetHost := &fakeHost{deploy: func(context.Context, workload.Definition) (host.Result, error) {
		return host.Result{}, operation
	}}
	provider := providerOf(targetHost)
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	command.Writer = &failingWriter{}

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, operation) {
		t.Fatalf("error = %v", err)
	}
	// The provider is closed even when the operation fails.
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
}

func TestCommandJoinsCloseFailureWithoutMaskingOperationError(t *testing.T) {
	operation := errors.New("connect failed")
	provider := providerOf(&fakeHost{deploy: func(context.Context, workload.Definition) (host.Result, error) {
		return host.Result{}, operation
	}})
	provider.closeErr = errors.New("session close failed")
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	command.Writer = io.Discard

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, operation) || !strings.Contains(err.Error(), "session close failed") {
		t.Fatalf("error = %v", err)
	}
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
}

func TestDeploySurfacesPreflightFailureWithoutMutating(t *testing.T) {
	preflight := errors.New("check Sunbeam access: sunbeam control refused")
	// deploy stays unset: the command must not mutate after a failed check.
	targetHost := &fakeHost{check: func(context.Context, string, bool) error {
		return preflight
	}}
	provider := providerOf(targetHost)
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	var output bytes.Buffer
	command.Writer = &output

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, preflight) {
		t.Fatalf("error = %v", err)
	}
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
	// The summary renders the preflight failure with no target completed
	// and nothing claimed as attempted-and-done.
	if !strings.Contains(output.String(), "storefront / web-1 (us-central1-a): failed") {
		t.Fatalf("output %q does not show the failed target", output.String())
	}
	if strings.Contains(output.String(), "completed") {
		t.Fatalf("preflight failure summary shows a completed target: %q", output.String())
	}
}

func TestDeploySurfacesProgressWriteFailureAfterSuccessfulOperation(t *testing.T) {
	var openedReporter host.Reporter
	deployed := false
	provider := &fakeProvider{open: func(_ context.Context, _ deployment.Target, report host.Reporter) (deployment.Handle, error) {
		openedReporter = report

		return deployment.Handle{
			Host: &fakeHost{deploy: func(context.Context, workload.Definition) (host.Result, error) {
				deployed = true
				openedReporter(host.Progress{Stage: host.StageHealthy})

				return host.Result{}, nil
			}},
			Close: func() error { return nil },
		}, nil
	}}
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	writer := &failingWriter{}
	command.Writer = writer

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "progress write failed") {
		t.Fatalf("error = %v", err)
	}
	if writer.calls == 0 {
		t.Fatal("no progress was reported")
	}
	// The broken writer never interrupted the operation: the host ran to
	// completion and its result summary was still attempted.
	if !deployed {
		t.Fatal("display failure interrupted the operation")
	}
}

func TestDeployRendersResultsEvenOnFailure(t *testing.T) {
	// The operation fails, but its result summary still renders every
	// selected target, the failing error stays the command's error, and
	// the provider is cleaned up.
	operation := errors.New("update route: not ready")
	provider := providerOf(&fakeHost{
		check: func(context.Context, string, bool) error { return nil },
		deploy: func(context.Context, workload.Definition) (host.Result, error) {
			return host.Result{}, operation
		},
	})
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	var output bytes.Buffer
	command.Writer = &output

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, operation) {
		t.Fatalf("error = %v", err)
	}
	for _, expected := range []string{
		"storefront / web-1 (us-central1-a): failed",
		"  error: " + operation.Error(),
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output %q does not contain %q", output.String(), expected)
		}
	}
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
}

func TestCommandKeepsOperationDisplayAndCloseErrorsSeparate(t *testing.T) {
	// Operation failure, display failure, and cleanup failure all surface
	// in one joined error without masking one another, and the display
	// failure is labeled so it never reads as a workload failure.
	operation := errors.New("connect failed")
	closeFailure := errors.New("session close failed")
	provider := providerOf(&fakeHost{
		check: func(context.Context, string, bool) error { return nil },
		deploy: func(context.Context, workload.Definition) (host.Result, error) {
			return host.Result{}, operation
		},
	})
	provider.closeErr = closeFailure
	command := commandWith(func(string) (commandProvider, error) {
		return provider, nil
	})
	command.Writer = &failingWriter{}

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", writeWorkload(t, t.TempDir(), "storefront", "web-1"),
		"--impersonate-service-account", "operator@example.com",
	})
	if !errors.Is(err, operation) {
		t.Fatalf("operation failure masked: %v", err)
	}
	if !errors.Is(err, closeFailure) {
		t.Fatalf("close failure masked: %v", err)
	}
	if !strings.Contains(err.Error(), "write output: progress write failed") {
		t.Fatalf("display failure unlabeled: %v", err)
	}
	if provider.closes != 1 {
		t.Fatalf("provider closes = %d", provider.closes)
	}
}

func TestDeployRejectsInvalidWorkloadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(path, []byte("name: broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	factories := 0
	command := commandWith(func(string) (commandProvider, error) {
		factories++

		return providerOf(&fakeHost{}), nil
	})
	command.Writer = io.Discard

	err := command.Run(context.Background(), []string{
		"sunstone", "deploy",
		"-f", path,
		"--impersonate-service-account", "operator@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("error = %v", err)
	}
	// A malformed file is rejected before any connection resources exist.
	if factories != 0 {
		t.Fatalf("provider factories = %d", factories)
	}
}

// writeWorkload stores one valid single-instance workload file and returns
// its path.
func writeWorkload(t *testing.T, directory, workloadName, instance string) string {
	t.Helper()
	path := filepath.Join(directory, workloadName+".yaml")
	document := fmt.Sprintf(`name: %s
gcp:
  project: acme-prod
  instances:
    - name: %s
      zone: us-central1-a
container:
  image: docker.io/example/web:v1
`, workloadName, instance)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// providerOf opens the same host for every target.
func providerOf(targetHost deployment.Host) *fakeProvider {
	return &fakeProvider{open: func(context.Context, deployment.Target, host.Reporter) (deployment.Handle, error) {
		return deployment.Handle{Host: targetHost, Close: func() error { return nil }}, nil
	}}
}

// fakeProvider scripts the command's closable provider seam.
type fakeProvider struct {
	open     func(context.Context, deployment.Target, host.Reporter) (deployment.Handle, error)
	closeErr error
	closes   int
}

func (p *fakeProvider) Open(ctx context.Context, target deployment.Target, report host.Reporter) (deployment.Handle, error) {
	return p.open(ctx, target, report)
}

func (p *fakeProvider) Close() error {
	p.closes++

	return p.closeErr
}

// fakeHost scripts high-level host results for command tests. An unset
// check succeeds; every other unset function panics when called.
type fakeHost struct {
	check   func(context.Context, string, bool) error
	deploy  func(context.Context, workload.Definition) (host.Result, error)
	status  func(context.Context, string) (host.Observation, error)
	restart func(context.Context, string) (host.Result, error)
	remove  func(context.Context, string) (host.Result, error)
}

func (h *fakeHost) Check(ctx context.Context, workloadName string, desiredHTTP bool) error {
	if h.check == nil {
		return nil
	}

	return h.check(ctx, workloadName, desiredHTTP)
}

func (h *fakeHost) Deploy(ctx context.Context, definition workload.Definition) (host.Result, error) {
	return h.deploy(ctx, definition)
}

func (h *fakeHost) Status(ctx context.Context, workloadName string) (host.Observation, error) {
	return h.status(ctx, workloadName)
}

func (h *fakeHost) Restart(ctx context.Context, workloadName string) (host.Result, error) {
	return h.restart(ctx, workloadName)
}

func (h *fakeHost) Remove(ctx context.Context, workloadName string) (host.Result, error) {
	return h.remove(ctx, workloadName)
}

type failingWriter struct {
	calls int
}

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++

	return 0, errors.New("progress write failed")
}

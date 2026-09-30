//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// This opt-in test uses the primary Docker Go client -> Sunray executable seam.
// Provision and register the trusted runtime first. Pass only a reference and a
// SHA-256 digest of a disposable test payload, never the payload itself.
func TestDockerRuntimeCreatesExecutesAndChecksSecretWorkload(t *testing.T) {
	if os.Getenv("SUNRAY_DOCKER_TEST") != "1" {
		t.Skip("requires registered Sunray and disposable Secret Manager reference")
	}
	reference, expected := os.Getenv("SUNRAY_TEST_REFERENCE"), os.Getenv("SUNRAY_TEST_SHA256")
	if reference == "" || len(expected) != 64 {
		t.Fatal("test reference and SHA-256 are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker, err := client.New(client.WithHostFromEnv(), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	check := `test "${API_KEY+x}" = x && test "$(printf %s "$API_KEY" | sha256sum | cut -d' ' -f1)" = "$EXPECTED_SHA256"`
	annotation, _ := json.Marshal(map[string]string{"API_KEY": reference})
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "sunray-integration-workload",
		Config:     &container.Config{Image: "busybox:1.37.0", User: "65532:65532", Env: []string{"EXPECTED_SHA256=" + expected}, Cmd: []string{"sh", "-c", check + ` || exit 23; echo injected=true; exec sleep 300`}, Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", check}, Interval: time.Second, Timeout: 5 * time.Second, Retries: 3}},
		HostConfig: &container.HostConfig{Runtime: "sunray", Annotations: map[string]string{"dev.sunstone.secret-env": string(annotation)}, ReadonlyRootfs: true, CapDrop: []string{"ALL"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		docker.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	for {
		inspected, err := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range inspected.Container.Config.Env {
			if strings.HasPrefix(entry, "API_KEY=") {
				t.Fatal("secret stored in Docker metadata")
			}
		}
		if !inspected.Container.State.Running {
			t.Fatal("workload did not stay running")
		}
		if health := inspected.Container.State.Health; health != nil && string(health.Status) == "healthy" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Docker health check did not receive secret")
		case <-time.After(100 * time.Millisecond):
		}
	}
	exec, err := docker.ExecCreate(ctx, created.ID, client.ExecCreateOptions{Cmd: []string{"sh", "-c", check + ` && test "$KEEP" = exec`}, Env: []string{"KEEP=exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docker.ExecStart(ctx, exec.ID, client.ExecStartOptions{Detach: true}); err != nil {
		t.Fatal(err)
	}
	for {
		result, err := docker.ExecInspect(ctx, exec.ID, client.ExecInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Running {
			if result.ExitCode != 0 {
				t.Fatalf("exec environment check exited %d", result.ExitCode)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("exec did not finish")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Missing authorized version fails at start, not at metadata creation.
	prefix, _, ok := strings.Cut(reference, "/versions/")
	if !ok {
		t.Fatal("invalid test reference")
	}
	annotation, _ = json.Marshal(map[string]string{"API_KEY": prefix + "/versions/999999"})
	failed, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{Name: "sunray-integration-missing", Config: &container.Config{Image: "busybox:1.37.0", Cmd: []string{"sleep", "300"}}, HostConfig: &container.HostConfig{Runtime: "sunray", Annotations: map[string]string{"dev.sunstone.secret-env": string(annotation)}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		docker.ContainerRemove(context.Background(), failed.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := docker.ContainerStart(ctx, failed.ID, client.ContainerStartOptions{}); err == nil || !strings.Contains(err.Error(), "HTTP 404") || !errdefs.IsInternal(err) {
		t.Fatalf("missing-version error: %v", err)
	}
}

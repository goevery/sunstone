//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecUsesContainerSnapshotAndRestartRefreshesAlias(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("latest")
	if output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container"); err != nil {
		t.Fatalf("create: %v %s", err, output)
	}
	f.version.Store(2)
	process := filepath.Join(f.bundle, "exec.json")
	original := `{"args":["diagnostic"],"env":["KEEP=exec-literal","API_KEY=override"],"future":{"preserve":true}}`
	if err := os.WriteFile(process, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("--root="+f.root, "exec", "--process", process, "container")
	if err != nil || !strings.Contains(string(output), "API_KEY=test-secret") || !strings.Contains(string(output), "KEEP=exec-literal") || strings.Contains(string(output), "rotated-secret") {
		t.Fatalf("exec: %v %s", err, output)
	}
	unchanged, _ := os.ReadFile(process)
	if string(unchanged) != original {
		t.Fatal("exec input changed")
	}
	if output, err = f.run("--root", f.root, "create", "--bundle", f.bundle, "container"); err != nil || !strings.Contains(string(output), "rotated-secret") {
		t.Fatalf("restart: %v %s", err, output)
	}
	// An inline process option must be rewritten just like a separate value.
	// The fixture's delegated runtime understands only separate values here.
	output, err = f.run("--root", f.root, "exec", "-p", process, "container")
	if err != nil || !strings.Contains(string(output), "API_KEY=rotated-secret") {
		t.Fatalf("refreshed exec: %v %s", err, output)
	}
}

func TestDeniedExecDoesNotDiscardContainerSnapshot(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("latest")
	if output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container"); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	process := filepath.Join(f.bundle, "exec.json")
	if err := os.WriteFile(process, []byte(`{"args":["diagnostic"],"env":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.failure.Store(403)
	if output, err := f.run("--root", f.root, "exec", "-p", process, "container"); err == nil || !strings.Contains(string(output), "HTTP 403") {
		t.Fatalf("%v %s", err, output)
	}
	f.failure.Store(0)
	f.version.Store(2)
	if output, err := f.run("--root", f.root, "exec", "-p", process, "container"); err != nil || !strings.Contains(string(output), "API_KEY=test-secret") {
		t.Fatalf("snapshot lost: %v %s", err, output)
	}
}

func (f *runtimeFixture) writeBundle(version string) {
	f.t.Helper()
	data := fmt.Sprintf(`{"annotations":{"dev.sunstone.secret-env":"{\"API_KEY\":\"projects/p/secrets/key/versions/%s\"}"},"process":{"args":["workload"],"env":[]}}`, version)
	if err := os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}

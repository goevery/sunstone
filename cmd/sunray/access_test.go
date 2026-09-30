//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnavailableMetadataCannotFallBackToOperatorCredentials(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	f.server.Close()
	command := f.command("--root", f.root, "create", "-b", f.bundle, "container")
	command.Env = append(command.Env, "GOOGLE_APPLICATION_CREDENTIALS=/no/operator/credentials")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "fetch VM identity token") || f.accesses.Load() != 0 {
		t.Fatalf("%v %s", err, output)
	}
}

func TestPreparationDeadlinePreventsStartingWithPartialSecrets(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	command := f.command("--root", f.root, "create", "-b", f.bundle, "container")
	command.Env = append(command.Env, "TEST_TIMEOUT=10ms")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "request failed") {
		t.Fatalf("%v %s", err, output)
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if string(restored) != string(original) {
		t.Fatal("deadline exposed a partial environment")
	}
}

func TestCanonicalProjectNumberIsPinnedForExec(t *testing.T) {
	f := newRuntimeFixture(t)
	f.canonical.Store(true)
	f.writeBundle("latest")
	output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container")
	if err != nil {
		t.Fatalf("canonical response: %v %s", err, output)
	}
	process := filepath.Join(f.bundle, "exec.json")
	if err := os.WriteFile(process, []byte(`{"env":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.version.Store(2)
	output, err = f.run("--root", f.root, "exec", "-p", process, "container")
	if err != nil || !strings.Contains(string(output), "API_KEY=test-secret") {
		t.Fatalf("canonical snapshot: %v %s", err, output)
	}
}

func TestUniqueReferencesAreAccessedConcurrentlyAndDeduplicated(t *testing.T) {
	f := newRuntimeFixture(t)
	bundle := `{"annotations":{"dev.sunstone.secret-env":"{\"ONE\":\"projects/p/secrets/key/versions/1\",\"ALSO_ONE\":\"projects/p/secrets/key/versions/1\",\"TWO\":\"projects/p/secrets/key/versions/2\"}"},"process":{"env":[]}}`
	if err := os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(bundle), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container")
	if err != nil || f.accesses.Load() != 2 || f.peak.Load() < 2 || !strings.Contains(string(output), "TWO=rotated-secret") {
		t.Fatalf("err=%v accesses=%d concurrency=%d output=%s", err, f.accesses.Load(), f.peak.Load(), output)
	}
}

func TestTemporarySecretFailureIsRetriedWithoutRetryingPermanentFailures(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	f.temporary.Store(1)
	output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container")
	if err != nil || f.accesses.Load() != 2 {
		t.Fatalf("%v accesses=%d %s", err, f.accesses.Load(), output)
	}
	f.failure.Store(403)
	f.accesses.Store(0)
	output, err = f.run("--root", f.root, "create", "-b", f.bundle, "other-container")
	if err == nil || f.accesses.Load() != 1 {
		t.Fatalf("%v accesses=%d %s", err, f.accesses.Load(), output)
	}
}

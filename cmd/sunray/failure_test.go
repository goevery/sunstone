//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreparationFailuresAreSafeAndReachRuntimeJSONLog(t *testing.T) {
	for _, kind := range []string{"denied", "missing", "checksum", "malformed", "duplicate", "persistent"} {
		t.Run(kind, func(t *testing.T) {
			f := newRuntimeFixture(t)
			f.writeBundle("latest")
			expected := "HTTP 403"
			switch kind {
			case "denied":
				f.failure.Store(403)
			case "missing":
				f.failure.Store(404)
				expected = "HTTP 404"
			case "checksum":
				f.corrupt.Store(true)
				expected = "checksum mismatch"
			case "malformed":
				os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(`{"annotations":{"dev.sunstone.secret-env":"null"}}`), 0600)
				expected = "secret annotation"
			case "duplicate":
				os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(`{"annotations":{"dev.sunstone.secret-env":"{\"KEY\":\"projects/p/secrets/s/versions/1\",\"KEY\":\"projects/p/secrets/s/versions/2\"}"}}`), 0600)
				expected = "duplicate"
			case "persistent":
				data, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
				f.bundle, _ = os.MkdirTemp(".", ".sunray-persistent-test-")
				t.Cleanup(func() { os.RemoveAll(f.bundle) })
				os.WriteFile(filepath.Join(f.bundle, "config.json"), data, 0600)
				expected = "tmpfs"
			}
			original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
			log := filepath.Join(f.bundle, "log.json")
			output, err := f.run("--log", log, "--log-format", "json", "--root", f.root, "create", "-b", f.bundle, "container")
			if err == nil || !strings.Contains(string(output), expected) {
				t.Fatalf("%v %s", err, output)
			}
			logged, _ := os.ReadFile(log)
			var entry map[string]string
			if err := json.Unmarshal(logged, &entry); err != nil || entry["level"] != "error" || !strings.Contains(entry["msg"], expected) {
				t.Fatalf("invalid diagnostic: %s", logged)
			}
			if strings.Contains(string(logged), "test-secret") || strings.Contains(string(output), "vm-test-token") {
				t.Fatal("sensitive response leaked")
			}
			restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
			if string(restored) != string(original) {
				t.Fatal("failure changed bundle")
			}
		})
	}
}

func TestLargeValidBundleRestoresExactOriginalBytes(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	original = append([]byte(`{"future":"`+strings.Repeat("a", 1600*1024)+`",`), original[1:]...)
	if err := os.WriteFile(filepath.Join(f.bundle, "config.json"), original, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.bundle, "config.json"), 0640); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container"); err != nil {
		t.Fatalf("%v output bytes=%d", err, len(output))
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	info, _ := os.Stat(filepath.Join(f.bundle, "config.json"))
	if string(restored) != string(original) || info.Mode().Perm() != 0640 {
		t.Fatal("large bundle restoration changed bytes or mode")
	}
}

func TestDelegatedExitRestoresBundleAndDoesNotLeaveSnapshot(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("latest")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if err := os.WriteFile(f.delegate, []byte("#!/bin/sh\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container")
	if err == nil || !strings.Contains(err.Error(), "exit status 42") {
		t.Fatalf("%v %s", err, output)
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if string(original) != string(restored) {
		t.Fatal("bundle not restored")
	}
	filepath.WalkDir(f.state, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Name() == "snapshot.json" {
			t.Error("failed creation left snapshot")
		}
		return err
	})
}

func TestFailedRestorationRetainsRecoveryJournalForNextInvocation(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	script := "#!/bin/sh\nrm '" + f.bundle + "/config.json'\nmkdir '" + f.bundle + "/config.json'\n"
	if err := os.WriteFile(f.delegate, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container"); err == nil || !strings.Contains(string(output), "restore original") {
		t.Fatalf("%v %s", err, output)
	}
	if err := os.Remove(filepath.Join(f.bundle, "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.delegate, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run("--root", f.root, "delete", "container"); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if string(restored) != string(original) {
		t.Fatal("failed restoration lost original bundle")
	}
}

func TestUncatchableInterruptionIsRecoveredBeforeNextOperation(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("latest")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	delegate, _ := os.ReadFile(f.delegate)
	script := "#!/bin/sh\ntouch '" + f.bundle + "/entered'\nexec sleep 60\n"
	if err := os.WriteFile(f.delegate, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command := f.command("--root", f.root, "create", "-b", f.bundle, "container")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			command.Process.Kill()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.bundle, "entered")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delegated runtime never entered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	modified, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if !strings.Contains(string(modified), "API_KEY=test-secret") {
		t.Fatal("did not interrupt injected specification")
	}
	orphan := filepath.Join(f.bundle, ".sunray-orphan")
	if err := os.WriteFile(orphan, []byte("API_KEY=test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	if err := os.WriteFile(f.delegate, delegate, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run("--root", f.root, "delete", "--force", "container"); err != nil {
		t.Fatalf("recovery: %v %s", err, output)
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if string(restored) != string(original) {
		t.Fatal("interrupted bundle not recovered")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned preparation payload not removed")
	}
}

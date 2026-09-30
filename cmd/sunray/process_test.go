//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func processEnvironment(t *testing.T, data []byte) map[string]string {
	t.Helper()
	var spec struct{ Process struct{ Env []string } }
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("%v %s", err, data)
	}
	result := make(map[string]string)
	for _, entry := range spec.Process.Env {
		name, value, _ := strings.Cut(entry, "=")
		result[name] = value
	}
	return result
}

func TestEnvironmentPayloadsArePreservedOrRejectedLosslessly(t *testing.T) {
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{"empty", "", true}, {"multiline", "  line one\nline=two\"  ", true}, {"nul", "a\x00b", false}, {"invalid-utf8", string([]byte{0xff}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntimeFixture(t)
			f.writeBundle("1")
			f.payload.Store([]byte(test.value))
			output, err := f.run("--root", f.root, "create", "-b", f.bundle, "container")
			if test.valid {
				if err != nil {
					t.Fatalf("%v %s", err, output)
				}
				environment := processEnvironment(t, output)
				if environment["API_KEY"] != test.value {
					t.Fatal("payload changed")
				}
			} else if err == nil || !strings.Contains(string(output), "UTF-8 without NUL") {
				t.Fatalf("%v %s", err, output)
			}
		})
	}
}

func TestOrdinaryRuntimeCommandsPreserveExitStatusWithoutSecretAccess(t *testing.T) {
	f := newRuntimeFixture(t)
	if err := os.WriteFile(f.delegate, []byte("#!/bin/sh\nprintf '%s|' \"$@\"\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("--log-format=json", "features")
	if err == nil || !strings.Contains(err.Error(), "exit status 42") || string(output) != "--log-format=json|features|" || f.tokens.Load() != 0 {
		t.Fatalf("%v %s tokens=%d", err, output, f.tokens.Load())
	}
}

func TestDelegationPreservesInheritedDescriptors(t *testing.T) {
	f := newRuntimeFixture(t)
	if err := os.WriteFile(f.delegate, []byte("#!/bin/sh\nprintf descriptor-preserved >&3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "descriptor"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	command := f.command("features")
	command.ExtraFiles = []*os.File{file}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	data, _ := os.ReadFile(file.Name())
	if string(data) != "descriptor-preserved" {
		t.Fatalf("inherited descriptor output=%s", data)
	}
}

func TestTerminationIsForwardedAndBundleRestored(t *testing.T) {
	f := newRuntimeFixture(t)
	f.writeBundle("1")
	original, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	script := "#!/bin/sh\ntrap 'exit 23' TERM\ntouch '" + f.bundle + "/entered'\nwhile :; do sleep 0.1; done\n"
	if err := os.WriteFile(f.delegate, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command := f.command("--root", f.root, "create", "-b", f.bundle, "container")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.bundle, "entered")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("delegated signal exit=%v", err)
	}
	restored, _ := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if string(restored) != string(original) {
		t.Fatal("signal bypassed restoration")
	}
}

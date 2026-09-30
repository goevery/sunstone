//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goevery/sunstone/internal/modules/sunray"
)

// Only the test executable recognizes these variables. The shipped executable
// has no environment hook for changing runtime or credential endpoints.
func TestRuntimeHelper(t *testing.T) {
	if os.Getenv("SUNRAY_TEST_HELPER") != "1" {
		return
	}
	runtime, err := sunray.New(sunray.Config{
		RuntimePath: os.Getenv("TEST_RUNTIME"), StateDirectory: os.Getenv("TEST_STATE"),
		MetadataEndpoint: os.Getenv("TEST_METADATA"), SecretEndpoint: os.Getenv("TEST_SECRETS"),
		Client: &http.Client{}, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	ctx := context.Background()
	if text := os.Getenv("TEST_TIMEOUT"); text != "" {
		duration, parseErr := time.ParseDuration(text)
		if parseErr != nil {
			panic(parseErr)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	code := 1
	if err == nil {
		code, err = runtime.Run(ctx, strings.Split(os.Getenv("TEST_ARGS"), "\n"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestCreateInjectsVMIdentitySecretsWithoutChangingBundle(t *testing.T) {
	f := newRuntimeFixture(t)
	original := `{"vendor":{"future":true},"annotations":{"dev.sunstone.secret-env":"{\"API_KEY\":\"projects/p/secrets/key/versions/latest\"}"},"process":{"args":["original"],"user":{"uid":65532},"env":["API_KEY=image-default","KEEP=literal"]}}`
	if err := os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := f.run("--root", f.root, "create", "--bundle", f.bundle, "container")
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	var observed struct {
		Process struct {
			Env  []string
			Args []string
		}
		Vendor map[string]bool
	}
	if err := json.Unmarshal(output, &observed); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if strings.Join(observed.Process.Env, "|") != "KEEP=literal|API_KEY=test-secret" || observed.Process.Args[0] != "original" || !observed.Vendor["future"] {
		t.Fatalf("unexpected delegated spec: %s", output)
	}
	restored, err := os.ReadFile(filepath.Join(f.bundle, "config.json"))
	if err != nil || string(restored) != original {
		t.Fatal("bundle not restored")
	}
}

type runtimeFixture struct {
	t                             *testing.T
	bundle, root, state, delegate string
	server                        *httptest.Server
	version                       atomic.Int64
	failure                       atomic.Int64
	accesses                      atomic.Int64
	corrupt                       atomic.Bool
	canonical                     atomic.Bool
	active, peak, temporary       atomic.Int64
	payload                       atomic.Value
	tokens                        atomic.Int64
}

func newRuntimeFixture(t *testing.T) *runtimeFixture {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "sunray-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &runtimeFixture{t: t, bundle: filepath.Join(dir, "bundle"), root: filepath.Join(dir, "runc"), state: filepath.Join(dir, "sunray"), delegate: filepath.Join(t.TempDir(), "runc")}
	if err := os.MkdirAll(f.bundle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.delegate, []byte(`#!/bin/sh
while [ "$1" != create ] && [ "$1" != exec ] && [ "$#" -gt 0 ]; do shift; done
command="$1"; shift
while [ "$#" -gt 0 ]; do
 case "$1" in
 --bundle|-b) cat "$2/config.json"; exit 0;;
 --process|-p) cat "$2"; exit 0;;
 esac
 shift
done
exit 0
`), 0700); err != nil {
		t.Fatal(err)
	}
	f.version.Store(1)
	f.payload.Store([]byte("test-secret"))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			f.tokens.Add(1)
			w.Header().Set("Metadata-Flavor", "Google")
			if r.Header.Get("Metadata-Flavor") != "Google" {
				t.Error("not metadata authentication")
			}
			fmt.Fprint(w, `{"access_token":"vm-test-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer vm-test-token" {
			t.Error("not VM token")
		}
		f.accesses.Add(1)
		active := f.active.Add(1)
		defer f.active.Add(-1)
		for peak := f.peak.Load(); active > peak; peak = f.peak.Load() {
			if f.peak.CompareAndSwap(peak, active) {
				break
			}
		}
		if f.temporary.Add(-1) >= 0 {
			w.WriteHeader(503)
			return
		}
		time.Sleep(20 * time.Millisecond)
		if status := f.failure.Load(); status != 0 {
			w.WriteHeader(int(status))
			fmt.Fprint(w, "test-secret vm-test-token")
			return
		}
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, ":access"), "/")
		version := parts[len(parts)-1]
		project := parts[3]
		if f.canonical.Load() {
			project = "123456789"
		}
		if version == "latest" {
			version = fmt.Sprint(f.version.Load())
		}
		data := f.payload.Load().([]byte)
		if version == "2" {
			data = []byte("rotated-secret")
		}
		checksum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
		if f.corrupt.Load() {
			checksum++
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "projects/" + project + "/secrets/key/versions/" + version, "payload": map[string]any{"data": data, "dataCrc32c": fmt.Sprint(checksum)}})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *runtimeFixture) run(args ...string) ([]byte, error) {
	f.t.Helper()
	return f.command(args...).CombinedOutput()
}

func (f *runtimeFixture) command(args ...string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestRuntimeHelper$")
	command.Env = append(os.Environ(), "SUNRAY_TEST_HELPER=1", "TEST_RUNTIME="+f.delegate, "TEST_STATE="+f.state, "TEST_METADATA="+f.server.URL+"/token", "TEST_SECRETS="+f.server.URL, "TEST_ARGS="+strings.Join(args, "\n"))
	return command
}

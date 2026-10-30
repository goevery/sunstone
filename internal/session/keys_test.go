package session

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testIdentity is a plausible service-account email used as the store's
// identity path component in tests.
const testIdentity = "deployer@sunstone-test.iam.gserviceaccount.com"

// subprocessDataHomeEnv names the environment variable that turns the test
// binary into a single LoadSigner subprocess for the concurrent-processes
// test. The value is the data home to load from.
const subprocessDataHomeEnv = "SUNSTONE_TEST_LOAD_SIGNER_DATA_HOME"

func TestMain(m *testing.M) {
	if dataHome := os.Getenv(subprocessDataHomeEnv); dataHome != "" {
		runLoadSignerSubprocess(dataHome)

		return
	}

	os.Exit(m.Run())
}

// runLoadSignerSubprocess loads the test identity's key under dataHome and
// prints its fingerprint on stdout, exiting nonzero on any error. It is the
// child half of TestLoadSignerConcurrentProcesses.
func runLoadSignerSubprocess(dataHome string) {
	signer, err := LoadSigner(dataHome, testIdentity)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println(ssh.FingerprintSHA256(signer.PublicKey()))
}

func keyPath(dataHome string) string {
	return filepath.Join(dataHome, "sunstone", "ssh", testIdentity, "id_ed25519")
}

func fingerprint(signer ssh.Signer) string {
	return ssh.FingerprintSHA256(signer.PublicKey())
}

func TestLoadSignerCreatesThenReuses(t *testing.T) {
	dataHome := t.TempDir()

	first, err := LoadSigner(dataHome, testIdentity)
	if err != nil {
		t.Fatalf("first LoadSigner: %v", err)
	}
	if want := keyPath(dataHome); !fileExists(t, want) {
		t.Fatalf("key was not created at %s", want)
	}

	second, err := LoadSigner(dataHome, testIdentity)
	if err != nil {
		t.Fatalf("second LoadSigner: %v", err)
	}
	if got, want := fingerprint(second), fingerprint(first); got != want {
		t.Fatalf("second call returned a different key: %s, want %s", got, want)
	}
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)

	return err == nil
}

// TestLoadSignerCreatesMissingDataHome verifies that a missing absolute
// data home is created, with owner-only permissions, before the key is
// stored under it.
func TestLoadSignerCreatesMissingDataHome(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "nested", "data")

	if _, err := LoadSigner(dataHome, testIdentity); err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	info, err := os.Stat(dataHome)
	if err != nil {
		t.Fatalf("stat created data home: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("created data home has mode %o, want 0700", perm)
	}
	if !fileExists(t, keyPath(dataHome)) {
		t.Fatalf("key was not created at %s", keyPath(dataHome))
	}
}

func TestLoadSignerPermissionModes(t *testing.T) {
	dataHome := t.TempDir()

	if _, err := LoadSigner(dataHome, testIdentity); err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	dirs := []string{
		filepath.Join(dataHome, "sunstone"),
		filepath.Join(dataHome, "sunstone", "ssh"),
		filepath.Dir(keyPath(dataHome)),
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("directory %s has mode %o, want 0700", dir, perm)
		}
	}

	for _, path := range []string{keyPath(dataHome), keyPath(dataHome) + ".lock"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file %s has mode %o, want 0600", path, perm)
		}
	}
}

func TestLoadSignerConcurrent(t *testing.T) {
	dataHome := t.TempDir()

	const workers = 16
	fingerprints := make([]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			signer, err := LoadSigner(dataHome, testIdentity)
			if err != nil {
				t.Errorf("concurrent LoadSigner: %v", err)

				return
			}
			fingerprints[i] = fingerprint(signer)
		}()
	}
	wg.Wait()

	for i, fp := range fingerprints {
		if fp == "" {
			t.Fatalf("worker %d produced no fingerprint", i)
		}
		if fp != fingerprints[0] {
			t.Fatalf("worker %d observed key %s, want %s", i, fp, fingerprints[0])
		}
	}
}

// TestLoadSignerConcurrentProcesses verifies the cross-process locking
// directly: several concurrently spawned test binaries must converge on one
// key file rather than each generating their own.
func TestLoadSignerConcurrentProcesses(t *testing.T) {
	dataHome := t.TempDir()

	const workers = 8
	outputs := make([]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), subprocessDataHomeEnv+"="+dataHome)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				t.Errorf("subprocess LoadSigner: %v", err)

				return
			}
			outputs[i] = strings.TrimSpace(out.String())
		}()
	}
	wg.Wait()

	for i, fp := range outputs {
		if fp == "" {
			t.Fatalf("subprocess %d produced no fingerprint", i)
		}
		if fp != outputs[0] {
			t.Fatalf("subprocess %d observed key %s, want %s", i, fp, outputs[0])
		}
	}
}

func TestLoadSignerRejectsMalformedKeyWithoutOverwrite(t *testing.T) {
	dataHome := t.TempDir()
	path := keyPath(dataHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	garbage := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot a real key\n")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}

	if _, err := LoadSigner(dataHome, testIdentity); err == nil {
		t.Fatal("LoadSigner accepted a malformed key")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back key: %v", err)
	}
	if !bytes.Equal(got, garbage) {
		t.Error("malformed key was overwritten")
	}
}

func TestLoadSignerRejectsSymlinkedIdentityDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "real-dir")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	dataHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataHome, "sunstone", "ssh"), 0o700); err != nil {
		t.Fatalf("mkdir ssh dir: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dataHome, "sunstone", "ssh", testIdentity)); err != nil {
		t.Fatalf("symlink identity dir: %v", err)
	}

	if _, err := LoadSigner(dataHome, testIdentity); err == nil {
		t.Fatal("symlinked identity directory accepted")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("files created through the symlinked identity directory: %v", entries)
	}
}

func TestLoadSignerRejectsInsecureAppDir(t *testing.T) {
	t.Run("world-readable sunstone", func(t *testing.T) {
		dataHome := t.TempDir()
		sunstone := filepath.Join(dataHome, "sunstone")
		if err := os.Mkdir(sunstone, 0o755); err != nil {
			t.Fatalf("mkdir sunstone: %v", err)
		}

		if _, err := LoadSigner(dataHome, testIdentity); err == nil {
			t.Fatal("world-readable app directory accepted")
		}

		info, err := os.Lstat(sunstone)
		if err != nil {
			t.Fatalf("lstat sunstone: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf("existing directory mode changed to %o", perm)
		}
		if entries, err := os.ReadDir(sunstone); err != nil {
			t.Fatalf("read sunstone: %v", err)
		} else if len(entries) > 0 {
			t.Errorf("files created under insecure directory: %v", entries)
		}
	})

	t.Run("group-writable identity", func(t *testing.T) {
		dataHome := t.TempDir()
		identityDir := filepath.Join(dataHome, "sunstone", "ssh", testIdentity)
		if err := os.MkdirAll(identityDir, 0o700); err != nil {
			t.Fatalf("mkdir identity dir: %v", err)
		}
		if err := os.Chmod(identityDir, 0o770); err != nil {
			t.Fatalf("chmod identity dir: %v", err)
		}

		if _, err := LoadSigner(dataHome, testIdentity); err == nil {
			t.Fatal("group-writable identity directory accepted")
		}

		info, err := os.Lstat(identityDir)
		if err != nil {
			t.Fatalf("lstat identity dir: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o770 {
			t.Errorf("existing directory mode changed to %o", perm)
		}
		if entries, err := os.ReadDir(identityDir); err != nil {
			t.Fatalf("read identity dir: %v", err)
		} else if len(entries) > 0 {
			t.Errorf("files created under insecure directory: %v", entries)
		}
	})
}

func TestLoadSignerRejectsSymlinkedLock(t *testing.T) {
	dataHome := t.TempDir()
	identityDir := filepath.Join(dataHome, "sunstone", "ssh", testIdentity)
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatalf("mkdir identity dir: %v", err)
	}

	sentinel := []byte("lock target contents")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, sentinel, 0o600); err != nil {
		t.Fatalf("write lock target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(identityDir, "id_ed25519.lock")); err != nil {
		t.Fatalf("symlink lock: %v", err)
	}

	if _, err := LoadSigner(dataHome, testIdentity); err == nil {
		t.Fatal("symlinked lock file accepted")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read lock target: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Error("symlinked lock target was modified")
	}
}

func TestLoadSignerRejectsSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "real-key")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}

	dataHome := t.TempDir()
	path := keyPath(dataHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := LoadSigner(dataHome, testIdentity); err == nil {
		t.Fatal("LoadSigner accepted a symlinked key")
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat key path: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced with a generated key")
	}
}

func TestLoadSignerRejectsUnsafePermissions(t *testing.T) {
	dataHome := t.TempDir()

	if _, err := LoadSigner(dataHome, testIdentity); err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	if err := os.Chmod(keyPath(dataHome), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := LoadSigner(dataHome, testIdentity); err == nil {
		t.Fatal("LoadSigner accepted a group/world-readable key")
	}
}

func TestLoadSignerRejectsUnsafeIdentity(t *testing.T) {
	for _, identity := range []string{
		"",
		".",
		"..",
		"../escape@x.com",
		"sub/dir@x.com",
		`back\slash@x.com`,
		"no-at-sign",
		"@nodomain",
		"user@nodot",
		"user@@double@x.com",
		"user@x.com/../..",
	} {
		dataHome := t.TempDir()
		if _, err := LoadSigner(dataHome, identity); err == nil {
			t.Errorf("identity %q was accepted", identity)
		}
		if entries, err := os.ReadDir(dataHome); err != nil {
			t.Errorf("read data home for %q: %v", identity, err)
		} else if len(entries) > 0 {
			t.Errorf("identity %q created files under the data home", identity)
		}

		// An invalid identity is rejected before a missing data home is
		// created for it.
		missing := filepath.Join(t.TempDir(), "absent")
		if _, err := LoadSigner(missing, identity); err == nil {
			t.Errorf("identity %q was accepted for a missing data home", identity)
		}
		if fileExists(t, missing) {
			t.Errorf("identity %q created its data home", identity)
		}
	}
}

func TestLoadSignerRejectsRelativeDataHome(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := LoadSigner("relative/data", testIdentity); err == nil {
		t.Fatal("relative data home was accepted")
	}
	if fileExists(t, "relative") {
		t.Fatal("relative data home was created")
	}
}

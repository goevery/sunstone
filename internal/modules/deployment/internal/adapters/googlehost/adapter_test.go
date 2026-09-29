package googlehost

import (
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestGeneratesUsableEphemeralKey(t *testing.T) {
	signer, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	challenge := []byte("sunstone")
	signature, err := signer.Sign(rand.Reader, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.PublicKey().Verify(challenge, signature); err != nil {
		t.Fatalf("generated key cannot verify its signature: %v", err)
	}
}

func TestTOFURejectsChangedHostKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ssh", "google_compute_known_hosts")
	alias := sshHostAlias(8845001922363177616)
	remote := &net.TCPAddr{IP: net.IPv4zero, Port: 22}
	first, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	callback, err := tofuHostKeyCallback(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback(alias, remote, first.PublicKey()); err != nil {
		t.Fatalf("trust first use: %v", err)
	}
	if err := callback(alias, remote, changed.PublicKey()); err == nil {
		t.Fatal("accepted changed host key")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := knownhosts.Line([]string{alias}, first.PublicKey()) + "\n"
	if string(contents) != want {
		t.Fatalf("known hosts = %q, want %q", contents, want)
	}
}

func TestBackgroundConfigurationFingerprintRemainsCompatible(t *testing.T) {
	fingerprint, err := configurationFingerprint(deploy.ContainerSpec{
		Image:         deploy.Image{ID: "sha256:image"},
		RestartPolicy: "unless-stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	const previousFingerprint = "a567ec426a494c90501fe4577041ae3bf2dedcd84e11371e52d720bf00f463b4"
	if fingerprint != previousFingerprint {
		t.Fatalf("background fingerprint = %q, want %q", fingerprint, previousFingerprint)
	}
}

func TestConfigurationFingerprintDoesNotExposeEnvironment(t *testing.T) {
	fingerprint, err := configurationFingerprint(deploy.ContainerSpec{
		Image:         deploy.Image{ID: "sha256:image"},
		Environment:   map[string]string{"PASSWORD": "super-secret"},
		RestartPolicy: "unless-stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprint) != 64 {
		t.Fatalf("fingerprint has length %d, want 64", len(fingerprint))
	}
	if fingerprint == "" || fingerprint == "super-secret" {
		t.Fatalf("invalid fingerprint %q", fingerprint)
	}
}

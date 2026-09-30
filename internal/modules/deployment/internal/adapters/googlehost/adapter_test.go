package googlehost

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/oauth2"
	"google.golang.org/api/compute/v1"
)

func TestReusesOneLoginIdentityAcrossTargetConnections(t *testing.T) {
	signer, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	adapter := &Adapter{signer: signer, identities: make(map[loginKey]*loginIdentity)}
	prepared := 0
	prepare := func(_ context.Context, serviceAccount string) (*loginIdentity, error) {
		prepared++
		if serviceAccount != "operator@example.com" {
			t.Fatalf("service account = %q", serviceAccount)
		}
		return &loginIdentity{}, nil
	}

	first, err := adapter.credentials(t.Context(), "acme-prod", "operator@example.com", prepare)
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.credentials(t.Context(), "acme-prod", "operator@example.com", prepare)
	if err != nil {
		t.Fatal(err)
	}
	imports := 0
	importKey := func(context.Context, oauth2.TokenSource, ssh.Signer, string, string) (string, error) {
		imports++
		return "operator", nil
	}
	if err := first.login(t.Context(), signer, "acme-prod", "operator@example.com", importKey); err != nil {
		t.Fatal(err)
	}
	if err := second.login(t.Context(), signer, "acme-prod", "operator@example.com", importKey); err != nil {
		t.Fatal(err)
	}
	if prepared != 1 || imports != 1 || first != second || first.username != "operator" {
		t.Fatalf("prepared = %d, imports = %d, identities = %p, %p", prepared, imports, first, second)
	}
}

func TestComputeLookupRunsWhileOSLoginKeyImports(t *testing.T) {
	importStarted := make(chan struct{})
	releaseImport := make(chan struct{})
	lookupStarted := make(chan struct{})
	identity := &loginIdentity{}
	importKey := func(context.Context, oauth2.TokenSource, ssh.Signer, string, string) (string, error) {
		close(importStarted)
		<-releaseImport
		return "operator", nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := prepareTarget(
			t.Context(), identity, nil, "acme-prod", "operator@example.com",
			func() (*compute.Instance, error) {
				close(lookupStarted)
				return &compute.Instance{Id: 1}, nil
			},
			importKey,
		)
		result <- err
	}()

	for name, started := range map[string]<-chan struct{}{"OS Login import": importStarted, "Compute lookup": lookupStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s did not start concurrently", name)
		}
	}
	close(releaseImport)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRetriesAuthenticationWithPreparedIdentity(t *testing.T) {
	attempts := 0
	connected, err := retrySSHAuthentication(t.Context(), func() (*host, error) {
		attempts++
		if attempts < 2 {
			return nil, errSSHAuthentication
		}
		return &host{}, nil
	})
	if err != nil || connected == nil || attempts != 2 {
		t.Fatalf("connected = %v, attempts = %d, error = %v", connected, attempts, err)
	}
}

func TestDoesNotRetryNonAuthenticationFailure(t *testing.T) {
	attempts := 0
	failure := errors.New("host key changed")
	_, err := retrySSHAuthentication(t.Context(), func() (*host, error) {
		attempts++
		return nil, failure
	})
	if !errors.Is(err, failure) || attempts != 1 {
		t.Fatalf("attempts = %d, error = %v", attempts, err)
	}
}

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

func TestHTTPContainerJoinsSunstoneNetworkWithoutHostPort(t *testing.T) {
	options := containerCreateOptions(deploy.ContainerSpec{
		HTTP: &deploy.HTTPConfig{ContainerPort: 8080},
	}, "storefront-0123456789ab-01234567", "fingerprint")
	if options.NetworkingConfig == nil || options.NetworkingConfig.EndpointsConfig[workloadNetwork] == nil {
		t.Fatalf("networking config = %+v", options.NetworkingConfig)
	}
	if len(options.HostConfig.PortBindings) != 0 {
		t.Fatalf("host port bindings = %+v", options.HostConfig.PortBindings)
	}
	if len(options.Config.ExposedPorts) != 1 {
		t.Fatalf("exposed ports = %+v", options.Config.ExposedPorts)
	}
}

func TestHTTPBackendUsesManagedContainerNetworkName(t *testing.T) {
	target := deploy.Container{Name: "storefront-0123456789ab-01234567"}
	address, err := (&host{}).BackendAddress(t.Context(), target, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if address != "storefront-0123456789ab-01234567:8080" {
		t.Fatalf("backend address = %q", address)
	}
}

func TestManagedContainerNameIsDNSLabel(t *testing.T) {
	name := managedContainerName(
		"a-very-long-workload-name-that-uses-the-maximum-allowed-label-length",
		"0123456789abcdef",
		"01234567",
	)
	if len(name) > maximumDNSLabel {
		t.Fatalf("container name has length %d: %q", len(name), name)
	}
	if name != "a-very-long-workload-name-that-uses-the-m-0123456789ab-01234567" {
		t.Fatalf("container name = %q", name)
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

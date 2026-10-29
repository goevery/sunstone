package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"
	"google.golang.org/api/compute/v1"
)

func TestNewGCPConnectorRejectsMissingCollaborators(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGCPConnector("", signer); err == nil {
		t.Fatal("accepted an empty known hosts path")
	}
	if _, err := NewGCPConnector(filepath.Join(t.TempDir(), "known_hosts"), nil); err == nil {
		t.Fatal("accepted a missing SSH signer")
	}
}

func TestNewGCPConnectorAcceptsExplicitCollaborators(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	connector, err := NewGCPConnector(filepath.Join(t.TempDir(), ".ssh", "google_compute_known_hosts"), signer)
	if err != nil {
		t.Fatal(err)
	}
	if connector.knownHostsPath == "" || connector.signer != signer || connector.identities == nil {
		t.Fatalf("connector = %+v", connector)
	}
}

func TestReusesOneLoginIdentityAcrossTargetConnections(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	connector := &GCPConnector{signer: signer, identities: make(map[loginKey]*loginIdentity)}
	prepared := 0
	prepare := func(_ context.Context, serviceAccount string) (*loginIdentity, error) {
		prepared++
		if serviceAccount != "operator@example.com" {
			t.Fatalf("service account = %q", serviceAccount)
		}
		return &loginIdentity{}, nil
	}

	first, err := connector.credentials(t.Context(), "acme-prod", "operator@example.com", prepare)
	if err != nil {
		t.Fatal(err)
	}
	second, err := connector.credentials(t.Context(), "acme-prod", "operator@example.com", prepare)
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
	connected, err := retrySSHAuthentication(t.Context(), func() (*Session, error) {
		attempts++
		if attempts < 2 {
			return nil, errSSHAuthentication
		}
		return &Session{}, nil
	})
	if err != nil || connected == nil || attempts != 2 {
		t.Fatalf("connected = %v, attempts = %d, error = %v", connected, attempts, err)
	}
}

func TestDoesNotRetryNonAuthenticationFailure(t *testing.T) {
	attempts := 0
	failure := errors.New("host key changed")
	_, err := retrySSHAuthentication(t.Context(), func() (*Session, error) {
		attempts++

		return nil, failure
	})
	if !errors.Is(err, failure) || attempts != 1 {
		t.Fatalf("attempts = %d, error = %v", attempts, err)
	}
}

package googlehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

func TestUniqueSecretReferencesAreDeduplicatedAndConcurrencyIsBounded(t *testing.T) {
	f := newSecretHost(t)
	f.delay.Store(int64(20 * time.Millisecond))
	spec := secretSpec()
	for version := 1; version <= 8; version++ {
		spec.SecretEnvironment[fmt.Sprintf("KEY_%d", version)] = fmt.Sprintf("projects/p/secrets/key/versions/%d", version)
	}
	spec.SecretEnvironment["DUPLICATE"] = spec.SecretEnvironment["API_KEY"]
	if _, err := f.host.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if f.accesses.Load() != 9 || f.tokens.Load() != 1 || f.peak.Load() < 2 || f.peak.Load() > 4 {
		t.Fatalf("accesses=%d tokens=%d concurrency=%d", f.accesses.Load(), f.tokens.Load(), f.peak.Load())
	}
}

func TestTransientSecretErrorsAreRetriedButPermanentErrorsAreNot(t *testing.T) {
	f := newSecretHost(t)
	f.transient.Store(1)
	if _, err := f.host.Create(t.Context(), secretSpec()); err != nil {
		t.Fatal(err)
	}
	if f.accesses.Load() != 2 {
		t.Fatalf("attempts=%d", f.accesses.Load())
	}
	denied := newSecretHost(t)
	denied.failure.Store(403)
	if _, err := denied.host.Create(t.Context(), secretSpec()); err == nil {
		t.Fatal("denied secret accepted")
	}
	if denied.accesses.Load() != 1 {
		t.Fatal("permanent failure was retried")
	}
}

func TestSecretAPITLSCertificateMustMatchItsHostname(t *testing.T) {
	f := newSecretHost(t)
	// Test-only trust configuration: an otherwise trusted certificate must still
	// be rejected when its DNS name does not match the expected server name.
	f.host.(*host).secretTransport.TLSClientConfig.ServerName = "wrong-api.invalid"
	if _, err := f.host.Create(t.Context(), secretSpec()); err == nil {
		t.Fatal("invalid server identity accepted")
	}
	if f.accesses.Load() != 0 || f.creates.Load() != 0 {
		t.Fatal("credentials or container reached an unverified endpoint")
	}
}

func TestSecretRequestsNeverFollowRedirectsOrUseOperatorProxies(t *testing.T) {
	// Redirecting to metadata would send the bearer credential to the wrong endpoint.
	f := newSecretHost(t)
	f.redirect.Store(true)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	if _, err := f.host.Create(t.Context(), secretSpec()); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect/proxy error=%v", err)
	}
	if f.tokens.Load() != 1 || f.accesses.Load() != 1 {
		t.Fatal("followed a redirect")
	}
}

func TestMetadataFailureDoesNotFallBackToOperatorIdentity(t *testing.T) {
	f := newSecretHost(t)
	f.metadataFailure.Store(403)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/no/operator/credentials")
	if _, err := f.host.Create(t.Context(), secretSpec()); err == nil || !strings.Contains(err.Error(), "fetch VM identity token") || strings.Contains(err.Error(), "vm-token") {
		t.Fatalf("metadata failure=%v", err)
	}
	if f.accesses.Load() != 0 || f.creates.Load() != 0 {
		t.Fatal("continued without VM credentials")
	}
}

func TestDockerCancellationPreservesContextCause(t *testing.T) {
	f := newSecretHost(t)
	f.dockerDelay.Store(int64(time.Second))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := f.host.Create(ctx, secretSpec()); result <- err }()
	select {
	case <-f.dockerEntered:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("Docker create request did not arrive")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Docker cancellation cause lost: %v", err)
	}
}

func TestCanceledSecretRequestDoesNotCreateContainer(t *testing.T) {
	f := newSecretHost(t)
	f.delay.Store(int64(time.Second))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := f.host.Create(ctx, secretSpec()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation=%v", err)
	}
	if f.creates.Load() != 0 {
		t.Fatal("canceled fetch created a container")
	}
}

func TestEmptyAndMultilineSecretValuesArePreserved(t *testing.T) {
	for _, value := range []string{"", " spaces\nquote\"=value "} {
		t.Run(fmt.Sprint(len(value)), func(t *testing.T) {
			f := newSecretHost(t)
			f.payload.Store(value)
			if _, err := f.host.Create(t.Context(), secretSpec()); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			actual, found := f.created.Config.Env["API_KEY"]
			f.mu.Unlock()
			if !found || actual != value {
				t.Fatal("payload was changed")
			}
		})
	}
}

func TestSecretCompareAndCreateUseOneSnapshotPerSSHConnection(t *testing.T) {
	f := newSecretHost(t)
	spec := secretSpec()
	candidate, err := f.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	f.payload.Store("rotated-value")
	matches, err := f.host.Matches(t.Context(), candidate, spec)
	if err != nil || !matches {
		t.Fatalf("existing connection changed snapshot: matches=%t err=%v", matches, err)
	}
	if _, err := f.host.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if f.accesses.Load() != 1 {
		t.Fatal("compare/create fetched alias again")
	}
	f.mu.Lock()
	prior := f.created
	f.mu.Unlock()
	next := newSecretHost(t)
	next.mu.Lock()
	next.created = prior
	next.mu.Unlock()
	next.payload.Store("rotated-value")
	matches, err = next.host.Matches(t.Context(), candidate, spec)
	if err != nil || matches {
		t.Fatalf("new deployment ignored rotation: matches=%t err=%v", matches, err)
	}
	if _, err := next.host.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	next.mu.Lock()
	value := next.created.Config.Env["API_KEY"]
	labels := next.created.Config.Labels
	next.mu.Unlock()
	if value != "rotated-value" || next.accesses.Load() != 1 {
		t.Fatal("new deployment did not use its resolved snapshot")
	}
	if labels[configurationLabel] != prior.Config.Labels[configurationLabel] {
		t.Fatal("resolved payload changed the configuration fingerprint")
	}
	encoded, _ := json.Marshal(labels)
	if strings.Contains(string(encoded), "rotated-value") {
		t.Fatal("secret entered labels")
	}
}

func TestChangingSecretReferenceReplacesContainerEvenWhenValueIsUnchanged(t *testing.T) {
	prior := newSecretHost(t)
	spec := secretSpec()
	spec.SecretEnvironment["API_KEY"] = "projects/p/secrets/key/versions/1"
	candidate, err := prior.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	prior.mu.Lock()
	configuration := prior.created
	prior.mu.Unlock()
	next := newSecretHost(t)
	next.mu.Lock()
	next.created = configuration
	next.mu.Unlock()
	spec.SecretEnvironment["API_KEY"] = "projects/p/secrets/key/versions/2"
	matches, err := next.host.Matches(t.Context(), candidate, spec)
	if err != nil || matches {
		t.Fatalf("changed secret reference was ignored: matches=%t err=%v", matches, err)
	}
}

func TestSecretFailuresPreventContainerCreationAndDoNotDiscloseResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int64
		corrupt  bool
		payload  string
		expected string
	}{
		{name: "denied", status: 403, expected: "HTTP 403"},
		{name: "missing", status: 404, expected: "HTTP 404"},
		{name: "checksum", corrupt: true, expected: "checksum mismatch"},
		{name: "nul", payload: "a\x00b", expected: "UTF-8 without NUL"},
		{name: "invalid-utf8", payload: string([]byte{0xff}), expected: "UTF-8 without NUL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSecretHost(t)
			f.failure.Store(test.status)
			f.corrupt.Store(test.corrupt)
			if test.payload != "" {
				f.payload.Store(test.payload)
			}
			_, err := f.host.Create(t.Context(), secretSpec())
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "vm-secret-value") || strings.Contains(err.Error(), "vm-token") {
				t.Fatal("error leaked secret or token")
			}
			if f.creates.Load() != 0 {
				t.Fatal("secret failure created a container")
			}
		})
	}
}

func TestSecretWorkloadLogsAreReturnedUnchanged(t *testing.T) {
	f := newSecretHost(t)
	value := "quote\"\nsecret"
	f.payload.Store(value)
	container, err := f.host.Create(t.Context(), secretSpec())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	escaped := string(encoded[1 : len(encoded)-1])
	expected := "failed with " + value + " and " + escaped
	f.mu.Lock()
	f.logs = expected
	f.mu.Unlock()
	logs, err := f.host.Logs(t.Context(), container)
	if err != nil {
		t.Fatal(err)
	}
	if logs != expected {
		t.Fatal("workload logs were suppressed or changed")
	}
}

func TestSecretPayloadsAreRedactedFromDockerErrors(t *testing.T) {
	f := newSecretHost(t)
	value := "quote\"\nsecret"
	f.payload.Store(value)
	f.dockerFailure.Store(true)
	_, err := f.host.Create(t.Context(), secretSpec())
	if err == nil || strings.Contains(err.Error(), value) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatal("Docker diagnostic exposed injected secret")
	}
}

func TestLiteralWorkloadsAndStoredContainerRestartsDoNotAccessSecrets(t *testing.T) {
	f := newSecretHost(t)
	spec := secretSpec()
	spec.SecretEnvironment = nil
	candidate, err := f.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.host.Start(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	// Starting an existing secret-enabled container also uses Docker's stored Env.
	if err := f.host.Start(t.Context(), deploy.Container{ID: "candidate"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != 0 || f.accesses.Load() != 0 {
		t.Fatal("ordinary creation or stored restart requested secrets")
	}
}

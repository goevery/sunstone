package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testControlAddress is the Sunbeam control endpoint used by the test fixture.
const testControlAddress = "127.0.0.1:2025"

func TestCreatesSecretEnvironmentUsingVMHTTPConnectionsOverSSH(t *testing.T) {
	fixture := newSecretHost(t)
	spec := secretSpec()
	candidate, err := fixture.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ID != "candidate" {
		t.Fatalf("container=%+v", candidate)
	}
	if _, found := spec.Environment["API_KEY"]; found {
		t.Fatal("resolved payload mutated the caller's workload configuration")
	}
	fixture.mu.Lock()
	created := fixture.created
	destinations := strings.Join(fixture.destinations, "|")
	fixture.mu.Unlock()
	if created.Config.Env["API_KEY"] != "vm-secret-value" || created.Config.Env["LITERAL"] != "keep" {
		t.Fatalf("resolved environment names missing")
	}
	if created.HostConfig.Runtime != "" || len(created.HostConfig.Annotations) != 0 {
		t.Fatal("secret resolution requires custom runtime")
	}
	if !strings.Contains(destinations, "169.254.169.254:80") || !strings.Contains(destinations, "secretmanager.googleapis.com:443") {
		t.Fatalf("HTTP did not use VM SSH forwarding: %s", destinations)
	}
	if fixture.tokens.Load() != 1 || fixture.accesses.Load() != 1 {
		t.Fatalf("tokens=%d accesses=%d", fixture.tokens.Load(), fixture.accesses.Load())
	}
}

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
	f.host.(*DockerRuntime).secretTransport.TLSClientConfig.ServerName = "wrong-api.invalid"
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
	if labels[SecretEnvironmentLabel] != prior.Config.Labels[SecretEnvironmentLabel] {
		t.Fatal("resolved payload changed the recorded secret references")
	}
	encoded, _ := json.Marshal(labels)
	if strings.Contains(string(encoded), "rotated-value") {
		t.Fatal("secret entered labels")
	}
}

func TestLegacyContainerWithoutAgreedMetadataIsNotCurrent(t *testing.T) {
	f := newSecretHost(t)
	spec := secretSpec()
	candidate, err := f.host.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.created.Config.Labels = map[string]string{
		ManagedLabel:                 "true",
		WorkloadLabel:                spec.Workload,
		"dev.sunstone.configuration": "a567ec426a494c90501fe4577041ae3bf2dedcd84e11371e52d720bf00f463b4",
	}
	f.mu.Unlock()

	matches, err := f.host.Matches(t.Context(), candidate, spec)
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("legacy metadata silently reported current")
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
	if err := f.host.Start(t.Context(), Container{ID: "candidate"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != 0 || f.accesses.Load() != 0 {
		t.Fatal("ordinary creation or stored restart requested secrets")
	}
}

type createdConfiguration struct {
	Config struct {
		Env    map[string]string
		Labels map[string]string
	}
	HostConfig struct {
		Runtime     string
		Annotations map[string]string
	}
}

type secretHostFixture struct {
	host                      Runtime
	transport                 sshTransport
	roots                     *x509.CertPool
	mu                        sync.Mutex
	created                   createdConfiguration
	logs                      string
	destinations              []string
	listed                    []map[string]any
	server                    *httptest.Server
	secretServer              *httptest.Server
	tokens, accesses, creates atomic.Int64
	inspectImage              atomic.Value
	inspectConfigImage        atomic.Value
	inspectCmd                atomic.Value
	inspectRestart            atomic.Value
	inspectRetries            atomic.Int64
	inspectNetwork            atomic.Bool
	failure                   atomic.Int64
	corrupt                   atomic.Bool
	dockerFailure             atomic.Bool
	dropCreate                atomic.Bool
	malformedCreate           atomic.Bool
	createNotFound            atomic.Bool
	createdRaw                []map[string]any
	dockerDelay               atomic.Int64
	dockerEntered             chan struct{}
	payload                   atomic.Value
	active, peak              atomic.Int64
	transient                 atomic.Int64
	delay                     atomic.Int64
	redirect                  atomic.Bool
	metadataFailure           atomic.Int64
}

func secretSpec() ContainerSpec {
	return ContainerSpec{Name: "jobs", Workload: "jobs", Image: Image{ID: "image"}, Environment: map[string]string{"LITERAL": "keep"}, SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/latest"}, RestartPolicy: "unless-stopped"}
}

// Exercise the real SSH handshake, direct-tcpip and streamlocal channels,
// TLS verification, HTTP clients, and Docker API. The SSH server is the only
// place that maps VM destinations to local test services.
func newSecretHost(t *testing.T) *secretHostFixture {
	t.Helper()
	f := &secretHostFixture{dockerEntered: make(chan struct{}, 1)}
	f.payload.Store("vm-secret-value")
	f.inspectImage.Store("image")
	f.inspectConfigImage.Store("image")
	f.inspectRestart.Store("unless-stopped")
	// The Sunbeam control endpoint accepts requests and drops the
	// connection before responding, proving the runtime treats the lost
	// answer as unconfirmed rather than failed.
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	go func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			io.Copy(io.Discard, conn)
			conn.Close()
		}
	}()
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokens.Add(1)
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/token" || r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("invalid metadata request")
		}

		if status := f.metadataFailure.Load(); status != 0 {
			w.WriteHeader(int(status))
			fmt.Fprint(w, "vm-token")

			return
		}

		w.Header().Set("Metadata-Flavor", "Google")
		fmt.Fprint(w, `{"access_token":"vm-token"}`)
	}))
	t.Cleanup(metadata.Close)
	f.secretServer = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.accesses.Add(1)
		active := f.active.Add(1)
		defer f.active.Add(-1)
		for peak := f.peak.Load(); active > peak; peak = f.peak.Load() {
			if f.peak.CompareAndSwap(peak, active) {
				break
			}
		}

		if delay := f.delay.Load(); delay != 0 {
			select {
			case <-time.After(time.Duration(delay)):
			case <-r.Context().Done():
				return
			}
		}

		if f.transient.Add(-1) >= 0 {
			w.WriteHeader(503)

			return
		}

		if f.redirect.Load() {
			w.Header().Set("Location", "http://169.254.169.254/redirected-token")
			w.WriteHeader(302)

			return
		}

		if r.Header.Get("Authorization") != "Bearer vm-token" {
			t.Error("secret request did not use VM identity")
		}

		if status := f.failure.Load(); status != 0 {
			w.WriteHeader(int(status))
			fmt.Fprint(w, "vm-secret-value vm-token")

			return
		}

		data := []byte(f.payload.Load().(string))
		checksum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
		if f.corrupt.Load() {
			checksum++
		}

		parts := strings.Split(strings.TrimSuffix(r.URL.Path, ":access"), "/")
		version := parts[len(parts)-1]
		if version == "latest" {
			version = "1"
		}

		json.NewEncoder(w).Encode(map[string]any{"name": "projects/123456/secrets/key/versions/" + version, "payload": map[string]any{"data": data, "dataCrc32c": fmt.Sprint(checksum)}})
	}))
	certificate, roots := secretCertificate(t)
	f.secretServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.secretServer.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	f.secretServer.StartTLS()
	t.Cleanup(f.secretServer.Close)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping") || r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.47")
			fmt.Fprint(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			select {
			case f.dockerEntered <- struct{}{}:
			default:
			}

			if f.dropCreate.Load() {
				// The request is accepted, then the response is dropped: the
				// client cannot know whether the daemon applied the create.
				f.creates.Add(1)
				hijackAndClose(t, w)

				return
			}

			if f.malformedCreate.Load() {
				// The daemon accepts the create and answers success with a
				// body the client cannot decode: the mutation may well be
				// applied, and the malformed answer proves nothing.
				f.creates.Add(1)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "not-json")

				return
			}

			if delay := f.dockerDelay.Load(); delay != 0 {
				select {
				case <-time.After(time.Duration(delay)):
				case <-r.Context().Done():
					return
				}
			}

			if f.createNotFound.Load() {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(map[string]string{"message": "No such image: sha256:missing"})

				return
			}

			if f.dockerFailure.Load() {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"message": "bad environment: " + f.payload.Load().(string)})

				return
			}

			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}

			var request struct {
				Env        []string
				Labels     map[string]string
				HostConfig struct {
					Runtime     string
					Annotations map[string]string
				}
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}

			raw := make(map[string]any)
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Error(err)
			}

			f.mu.Lock()
			f.createdRaw = append(f.createdRaw, raw)
			f.created.Config.Env = make(map[string]string)
			for _, entry := range request.Env {
				name, value, _ := strings.Cut(entry, "=")
				f.created.Config.Env[name] = value
			}

			f.created.Config.Labels = request.Labels
			f.created.HostConfig = request.HostConfig
			f.mu.Unlock()
			f.creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"candidate","Warnings":[]}`)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			f.mu.Lock()
			listed := f.listed
			f.mu.Unlock()
			json.NewEncoder(w).Encode(listed)
		case strings.HasSuffix(r.URL.Path, "/containers/candidate/json"):
			f.mu.Lock()
			defer f.mu.Unlock()
			var environment []string
			for name, value := range f.created.Config.Env {
				environment = append(environment, name+"="+value)
			}

			restartPolicy := map[string]any{"Name": f.inspectRestart.Load().(string)}
			if retries := f.inspectRetries.Load(); retries != 0 {
				restartPolicy["MaximumRetryCount"] = retries
			}

			inspected := map[string]any{
				"Id":         "candidate",
				"Image":      f.inspectImage.Load(),
				"Config":     map[string]any{"Image": f.inspectConfigImage.Load().(string), "Env": environment, "Labels": f.created.Config.Labels},
				"HostConfig": map[string]any{"RestartPolicy": restartPolicy},
			}
			if cmd, ok := f.inspectCmd.Load().([]string); ok {
				inspected["Config"].(map[string]any)["Cmd"] = cmd
			}

			if f.inspectNetwork.Load() {
				inspected["NetworkSettings"] = map[string]any{"Networks": map[string]any{workloadNetwork: map[string]any{}}}
			}
			json.NewEncoder(w).Encode(inspected)
		case strings.HasSuffix(r.URL.Path, "/containers/candidate/logs"):
			f.mu.Lock()
			logs := f.logs
			f.mu.Unlock()
			header := make([]byte, 8)
			header[0] = 1
			binary.BigEndian.PutUint32(header[4:], uint32(len(logs)))
			w.Write(header)
			io.WriteString(w, logs)
		case strings.HasSuffix(r.URL.Path, "/containers/candidate/start"):
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	hostKey, err := testSigner()
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := testSigner()
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(clientKey.PublicKey().Marshal()) {
			return nil, fmt.Errorf("unknown key")
		}
		return nil, nil
	}}
	config.AddHostKey(hostKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		tcp, err := listener.Accept()
		if err != nil {
			return
		}
		connection, channels, requests, err := ssh.NewServerConn(tcp, config)
		if err != nil {
			tcp.Close()

			return
		}

		defer connection.Close()
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			go func(channel ssh.NewChannel) {
				destination := ""
				target := ""
				switch channel.ChannelType() {
				case "direct-tcpip":
					var request struct {
						Host       string
						Port       uint32
						Origin     string
						OriginPort uint32
					}
					if err := ssh.Unmarshal(channel.ExtraData(), &request); err != nil {
						channel.Reject(ssh.ConnectionFailed, "invalid request")

						return
					}

					destination = fmt.Sprintf("%s:%d", request.Host, request.Port)
					switch destination {
					case "169.254.169.254:80":
						target = strings.TrimPrefix(metadata.URL, "http://")
					case "secretmanager.googleapis.com:443":
						target = strings.TrimPrefix(f.secretServer.URL, "https://")
					case testControlAddress:
						target = control.Addr().String()
					default:
						channel.Reject(ssh.Prohibited, "unexpected destination")

						return
					}
				case "direct-streamlocal@openssh.com":
					var request struct {
						Path, Reserved string
						Port           uint32
					}
					if err := ssh.Unmarshal(channel.ExtraData(), &request); err != nil || request.Path != "/var/run/docker.sock" {
						channel.Reject(ssh.Prohibited, "unexpected socket")

						return
					}

					destination = request.Path
					target = strings.TrimPrefix(f.server.URL, "http://")
				default:
					channel.Reject(ssh.UnknownChannelType, "no remote commands supported")

					return
				}

				f.mu.Lock()
				f.destinations = append(f.destinations, destination)
				f.mu.Unlock()
				backend, err := net.Dial("tcp", target)
				if err != nil {
					channel.Reject(ssh.ConnectionFailed, "unavailable")

					return
				}

				stream, requests, err := channel.Accept()
				if err != nil {
					backend.Close()

					return
				}

				go ssh.DiscardRequests(requests)
				go func() { io.Copy(stream, backend); stream.Close(); backend.Close() }()
				io.Copy(backend, stream)
				backend.Close()
				stream.Close()
			}(channel)
		}
	}()
	tcp, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	connection, channels, requests, err := ssh.NewClientConn(tcp, "test-vm", &ssh.ClientConfig{
		User:              "operator",
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback:   ssh.FixedHostKey(hostKey.PublicKey()),
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	})
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}

	transport := sshTransport{client: ssh.NewClient(connection, channels, requests)}
	connected, err := NewDockerRuntime(transport, testControlAddress)
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}

	// Trust the local API certificate without changing the production endpoints
	// or disabling hostname validation.
	connected.secretTransport.TLSClientConfig = &tls.Config{RootCAs: roots}
	f.host = connected
	f.transport = transport
	f.roots = roots
	// The runtime only borrows the transport; the fixture owns it and closes
	// it after the runtime's operation-scoped resources.
	t.Cleanup(func() {
		f.host.Close()
		transport.Close()
	})

	return f
}

// hijackAndClose takes over the response connection and closes it
// without answering, simulating an accepted request whose response is
// lost in transit.
func hijackAndClose(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("test server cannot hijack connections")

		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Error(err)

		return
	}
	conn.Close()
}

// forgetSecretState drops cached secret fetches and enrolled redaction
// values, proving that later behavior needs no prior secret activity.
func (f *secretHostFixture) forgetSecretState() {
	runtime := f.host.(*DockerRuntime)
	runtime.secretsMu.Lock()
	runtime.secretValues = make(map[string]string)
	runtime.enrolledSecrets = nil
	runtime.secretsMu.Unlock()
}

func TestOperationCloseClearsSecretStateAndKeepsTransportUsable(t *testing.T) {
	f := newSecretHost(t)
	if _, err := f.host.Create(t.Context(), secretSpec()); err != nil {
		t.Fatal(err)
	}
	transport := f.transport
	tokens := f.tokens.Load()

	// Enroll a deployed secret value for redaction, then finish the
	// operation: closing the runtime clears the fetched snapshot and the
	// enrolled values without closing the borrowed transport.
	runtime := f.host.(*DockerRuntime)
	runtime.enrollDeployedSecrets(
		[]string{"API_KEY=enrolled-secret"},
		map[string]string{"API_KEY": "projects/p/secrets/key/versions/1"},
	)
	if err := f.host.Close(); err != nil {
		t.Fatal(err)
	}
	if text := runtime.redact("leak: enrolled-secret vm-secret-value"); text != "leak: enrolled-secret vm-secret-value" {
		t.Fatalf("operation close retained secret state: %q", text)
	}

	// The transport outlives the operation: a fresh runtime over it is
	// usable and resolves its own snapshot instead of reusing the closed
	// operation's cache or redaction set.
	next, err := NewDockerRuntime(transport, testControlAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.secretTransport.TLSClientConfig = &tls.Config{RootCAs: f.roots}
	if _, err := next.Create(t.Context(), secretSpec()); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != tokens+1 {
		t.Fatal("new operation reused the previous operation's secret snapshot")
	}
}

// sshTransport adapts an SSH client to the host Session seam for tests; its
// Close belongs to the fixture, not to the runtimes borrowing it.
type sshTransport struct {
	client *ssh.Client
}

func (t sshTransport) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.client.DialContext(ctx, network, address)
}

func (t sshTransport) Close() error {
	return t.client.Close()
}

func testSigner() (ssh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	return ssh.NewSignerFromKey(private)
}

func secretCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test API"}, DNSNames: []string{"secretmanager.googleapis.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return certificate, roots
}

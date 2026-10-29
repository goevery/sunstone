package googlehost

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
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

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"golang.org/x/crypto/ssh"
)

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
	host                      deploy.Host
	mu                        sync.Mutex
	created                   createdConfiguration
	logs                      string
	destinations              []string
	server                    *httptest.Server
	secretServer              *httptest.Server
	tokens, accesses, creates atomic.Int64
	failure                   atomic.Int64
	corrupt                   atomic.Bool
	dockerFailure             atomic.Bool
	dockerDelay               atomic.Int64
	dockerEntered             chan struct{}
	payload                   atomic.Value
	active, peak              atomic.Int64
	transient                 atomic.Int64
	delay                     atomic.Int64
	redirect                  atomic.Bool
	metadataFailure           atomic.Int64
}

func secretSpec() deploy.ContainerSpec {
	return deploy.ContainerSpec{Name: "jobs", Workload: "jobs", Image: deploy.Image{ID: "image"}, Environment: map[string]string{"LITERAL": "keep"}, SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/latest"}, RestartPolicy: "unless-stopped"}
}

// Exercise the real SSH handshake, direct-tcpip and streamlocal channels,
// TLS verification, HTTP clients, and Docker API. The SSH server is the only
// place that maps VM destinations to local test services.
func newSecretHost(t *testing.T) *secretHostFixture {
	t.Helper()
	f := &secretHostFixture{dockerEntered: make(chan struct{}, 1)}
	f.payload.Store("vm-secret-value")
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
			if delay := f.dockerDelay.Load(); delay != 0 {
				select {
				case <-time.After(time.Duration(delay)):
				case <-r.Context().Done():
					return
				}
			}
			if f.dockerFailure.Load() {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"message": "bad environment: " + f.payload.Load().(string)})
				return
			}
			var request struct {
				Env        []string
				Labels     map[string]string
				HostConfig struct {
					Runtime     string
					Annotations map[string]string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
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
		case strings.HasSuffix(r.URL.Path, "/containers/candidate/json"):
			f.mu.Lock()
			defer f.mu.Unlock()
			var environment []string
			for name, value := range f.created.Config.Env {
				environment = append(environment, name+"="+value)
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "candidate", "Image": "image", "Config": map[string]any{"Env": environment, "Labels": f.created.Config.Labels}, "HostConfig": map[string]any{"RestartPolicy": map[string]string{"Name": "unless-stopped"}}})
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
	hostKey, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := generateKey()
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
	connected, err := openHost(tcp, clientKey, "operator", "test-vm", ssh.FixedHostKey(hostKey.PublicKey()))
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}
	// Trust the local API certificate without changing the production endpoints
	// or disabling hostname validation.
	connected.secretTransport.TLSClientConfig = &tls.Config{RootCAs: roots}
	f.host = connected
	t.Cleanup(func() { f.host.Close() })
	return f
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

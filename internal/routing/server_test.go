package routing_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"github.com/goevery/sunstone/internal/routing"
)

func TestAcceptsContainerInterfaceControlListener(t *testing.T) {
	traffic := listen(t)
	defer traffic.Close()
	control, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	server, err := newServer(t, traffic, control, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	if err != nil {
		t.Fatalf("create routing server: %v", err)
	}
	if server == nil {
		t.Fatal("routing server is nil")
	}
}

func TestNewServerRequiresCollaborators(t *testing.T) {
	router := newRouter(t, filepath.Join(t.TempDir(), "routes.json"))
	traffic := listen(t)
	defer traffic.Close()
	control := listen(t)
	defer control.Close()

	invalid := []routing.ServerConfig{
		{ControlListener: control, Router: router, ShutdownTimeout: time.Second},
		{TrafficListener: traffic, Router: router, ShutdownTimeout: time.Second},
		{TrafficListener: traffic, ControlListener: control, ShutdownTimeout: time.Second},
		{TrafficListener: traffic, ControlListener: control, Router: router},
	}
	for _, config := range invalid {
		if _, err := routing.NewServer(config); err == nil {
			t.Fatalf("accepted incomplete server configuration: %+v", config)
		}
	}
}

func TestNewRouterRequiresConfiguration(t *testing.T) {
	valid := routing.RouterConfig{
		StatePath:       filepath.Join(t.TempDir(), "routes.json"),
		ProbeTimeout:    time.Second,
		ProbeInterval:   time.Second,
		StartupDeadline: time.Second,
		DrainTimeout:    time.Second,
	}
	invalid := []routing.RouterConfig{
		{ProbeTimeout: valid.ProbeTimeout, ProbeInterval: valid.ProbeInterval, StartupDeadline: valid.StartupDeadline, DrainTimeout: valid.DrainTimeout},
		{StatePath: valid.StatePath, ProbeInterval: valid.ProbeInterval, StartupDeadline: valid.StartupDeadline, DrainTimeout: valid.DrainTimeout},
		{StatePath: valid.StatePath, ProbeTimeout: valid.ProbeTimeout, StartupDeadline: valid.StartupDeadline, DrainTimeout: valid.DrainTimeout},
		{StatePath: valid.StatePath, ProbeTimeout: valid.ProbeTimeout, ProbeInterval: valid.ProbeInterval, DrainTimeout: valid.DrainTimeout},
		{StatePath: valid.StatePath, ProbeTimeout: valid.ProbeTimeout, ProbeInterval: valid.ProbeInterval, StartupDeadline: valid.StartupDeadline},
	}
	for _, config := range invalid {
		if _, err := routing.NewRouter(config); err == nil {
			t.Fatalf("accepted incomplete router configuration: %+v", config)
		}
	}
}

func TestDeleteRouteStopsNewTrafficDrainsAndPersistsRemoval(t *testing.T) {
	admitted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/readyz":
			writer.WriteHeader(http.StatusNoContent)
		case "/hold":
			close(admitted)
			<-release
			_, _ = io.WriteString(writer, "complete")
		default:
			_, _ = io.WriteString(writer, "active")
		}
	}))
	defer backend.Close()

	traffic := listen(t)
	control := listen(t)
	statePath := filepath.Join(t.TempDir(), "routes.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveServer(t, traffic, control, statePath, time.Second, ctx) }()
	waitForHTTP(t, "http://"+control.Addr().String()+"/sunstone.sunbeam.v1.Sunbeam/GetRoute")

	client := sunbeampbconnect.NewSunbeamClient(http.DefaultClient, "http://"+control.Addr().String())
	if _, err := client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("container-1", backend.Listener.Addr().String()))); err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + traffic.Addr().String() + "/hold")
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		requestDone <- err
	}()
	<-admitted

	deleteDone := make(chan error, 1)
	go func() {
		_, err := client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront"}))
		deleteDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		response, err := http.Get("http://" + traffic.Addr().String())
		if err == nil && response.StatusCode == http.StatusServiceUnavailable {
			response.Body.Close()
			break
		}
		if err == nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("new traffic was not rejected during route deletion")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-deleteDone:
		t.Fatalf("delete returned before admitted request drained: %v", err)
	default:
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("persisted state still exists: %v", err)
	}
	if _, err := client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("deleted route error = %v", err)
	}
	if _, err := client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing route error = %v", err)
	}
	if _, err := client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront", AllowMissing: true})); err != nil {
		t.Fatalf("allow missing delete: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRejectsMalformedPersistedState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(statePath, []byte(`{"version":1,"route":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.NewRouter(routerConfig(statePath, time.Second)); err == nil {
		t.Fatal("expected malformed state to fail construction")
	}
}

func TestRoutesTrafficAfterCandidateStarts(t *testing.T) {
	type observedRequest struct {
		method     string
		host       string
		requestURI string
		forwarded  string
		body       string
	}
	observed := make(chan observedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, _ := io.ReadAll(request.Body)
		observed <- observedRequest{method: request.Method, host: request.Host, requestURI: request.URL.RequestURI(), forwarded: request.Header.Get("X-Forwarded-For"), body: string(body)}
		_, _ = io.WriteString(w, "candidate")
	}))
	defer backend.Close()

	traffic := listen(t)
	control := listen(t)
	statePath := filepath.Join(t.TempDir(), "routes.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveServer(t, traffic, control, statePath, time.Second, ctx) }()
	waitForHTTP(t, "http://"+control.Addr().String()+"/sunstone.sunbeam.v1.Sunbeam/GetRoute")

	response, err := http.Get("http://" + traffic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status before route = %d", response.StatusCode)
	}
	response.Body.Close()

	client := sunbeampbconnect.NewSunbeamClient(http.DefaultClient, "http://"+control.Addr().String())
	updated, err := client.UpdateRoute(context.Background(), connect.NewRequest(&sunbeampb.UpdateRouteRequest{
		Route: &sunbeampb.Route{
			Name: "routes/storefront",
			Backend: &sunbeampb.Backend{
				Address:          backend.Listener.Addr().String(),
				ContainerId:      "container-1",
				StartupProbePath: "/readyz",
			},
		},
		AllowMissing: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Msg.GetBackend().GetContainerId() != "container-1" {
		t.Fatalf("updated route = %+v", updated.Msg)
	}
	got, err := client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"}))
	if err != nil || got.Msg.GetBackend().GetContainerId() != "container-1" {
		t.Fatalf("get route = %+v, %v", got, err)
	}
	listed, err := client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{}))
	if err != nil || len(listed.Msg.GetRoutes()) != 1 {
		t.Fatalf("list routes = %+v, %v", listed, err)
	}
	different := routeRequest("container-2", backend.Listener.Addr().String())
	different.Route.Name = "routes/another"
	if _, err := client.UpdateRoute(context.Background(), connect.NewRequest(different)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second catch-all route error = %v", err)
	}

	proxyRequest, err := http.NewRequest(http.MethodPost, "http://"+traffic.Addr().String()+"/hello?from=test", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	proxyRequest.Host = "storefront.example"
	proxyRequest.Header.Set("X-Forwarded-For", "spoofed")
	response, err = http.DefaultClient.Do(proxyRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "candidate" {
		t.Fatalf("proxied response = %d %q", response.StatusCode, body)
	}
	request := <-observed
	if request.method != http.MethodPost || request.host != "storefront.example" || request.requestURI != "/hello?from=test" || request.body != "payload" || request.forwarded == "spoofed" || request.forwarded == "" {
		t.Fatalf("proxied request = %+v", request)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestForwardsStreamingAndWebSocketTraffic(t *testing.T) {
	releaseStream := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusNoContent)
		case "/stream":
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("backend response does not support flushing")
				return
			}
			_, _ = io.WriteString(w, "first\n")
			flusher.Flush()
			<-releaseStream
			_, _ = io.WriteString(w, "second\n")
		case "/socket":
			connection, err := websocket.Accept(w, request, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.CloseNow()
			messageType, message, err := connection.Read(request.Context())
			if err != nil {
				t.Error(err)
				return
			}
			if err := connection.Write(request.Context(), messageType, message); err != nil {
				t.Error(err)
			}
		}
	}))
	defer backend.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "container-1", backend.Listener.Addr().String())
	response, err := http.Get(running.traffic + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "first\n" {
		t.Fatalf("first streamed chunk = %q, %v", first, err)
	}
	close(releaseStream)
	second, err := reader.ReadString('\n')
	response.Body.Close()
	if err != nil || second != "second\n" {
		t.Fatalf("second streamed chunk = %q, %v", second, err)
	}

	websocketURL := "ws" + strings.TrimPrefix(running.traffic, "http") + "/socket"
	connection, _, err := websocket.Dial(context.Background(), websocketURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err := connection.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.Read(context.Background())
	if err != nil || string(message) != "hello" {
		t.Fatalf("websocket response = %q, %v", message, err)
	}
}

func TestFailedCandidateLeavesCurrentRouteActive(t *testing.T) {
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "current")
	}))
	defer current.Close()
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failed.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), 50*time.Millisecond)
	updateRoute(t, running.client, "container-current", current.Listener.Addr().String())
	_, err := running.client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("container-failed", failed.Listener.Addr().String())))
	// The startup probe exhausted its own deadline before any route
	// change: the rejection is definitive and identifies itself, so the
	// caller can safely clean up the failed candidate.
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("update error = %v", err)
	}
	if !strings.Contains(err.Error(), "backend failed startup probe") || !strings.Contains(err.Error(), "startup probe deadline exceeded") {
		t.Fatalf("update error lost its cause: %v", err)
	}
	if body := getBody(t, running.traffic); body != "current" {
		t.Fatalf("proxied body after failed update = %q", body)
	}
}

func TestCanceledStartupProbeStaysUnconfirmed(t *testing.T) {
	// Canceling the update's caller mid-probe proves nothing about the
	// candidate: the answer stays an ordinary cancellation instead of the
	// definitive startup-deadline rejection, and the current route keeps
	// serving.
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "current")
	}))
	defer current.Close()
	probed := make(chan struct{})
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probed)
		<-r.Context().Done()
	}))
	defer candidate.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), 5*time.Second)
	updateRoute(t, running.client, "container-current", current.Listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	update := make(chan error, 1)
	go func() {
		_, err := running.client.UpdateRoute(ctx, connect.NewRequest(routeRequest("container-new", candidate.Listener.Addr().String())))
		update <- err
	}()
	<-probed
	cancel()
	if err := <-update; connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("update error = %v", err)
	}
	if body := getBody(t, running.traffic); body != "current" {
		t.Fatalf("proxied body after canceled update = %q", body)
	}
}

func TestRouteUpdateDrainsAdmittedRequest(t *testing.T) {
	admitted := make(chan struct{})
	release := make(chan struct{})
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		close(admitted)
		<-release
		_, _ = io.WriteString(w, "current")
	}))
	defer current.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "replacement")
	}))
	defer replacement.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "container-current", current.Listener.Addr().String())
	oldResponse := make(chan string, 1)
	go func() { oldResponse <- getBody(t, running.traffic) }()
	<-admitted

	updated := make(chan error, 1)
	go func() {
		_, err := running.client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("container-new", replacement.Listener.Addr().String())))
		updated <- err
	}()
	select {
	case err := <-updated:
		t.Fatalf("update completed before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if body := <-oldResponse; body != "current" {
		t.Fatalf("admitted response = %q", body)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if body := getBody(t, running.traffic); body != "replacement" {
		t.Fatalf("new response = %q", body)
	}
}

func TestDrainDeadlineCancelsStuckRequest(t *testing.T) {
	admitted := make(chan struct{})
	canceled := make(chan struct{})
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		close(admitted)
		<-request.Context().Done()
		close(canceled)
	}))
	defer current.Close()
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "replacement")
	}))
	defer replacement.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "container-current", current.Listener.Addr().String())
	requestDone := make(chan struct{})
	go func() {
		response, _ := http.Get(running.traffic)
		if response != nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	<-admitted
	updateRoute(t, running.client, "container-new", replacement.Listener.Addr().String())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stuck backend request was not canceled")
	}
	<-requestDone
	if body := getBody(t, running.traffic); body != "replacement" {
		t.Fatalf("new response = %q", body)
	}
}

func TestRestoresPersistedRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "restored")
	}))
	defer backend.Close()
	statePath := filepath.Join(t.TempDir(), "routes.json")

	first := startRouting(t, statePath, time.Second)
	updateRoute(t, first.client, "container-1", backend.Listener.Addr().String())
	first.stop(t)

	second := startRouting(t, statePath, time.Second)
	if body := getBody(t, second.traffic); body != "restored" {
		t.Fatalf("restored body = %q", body)
	}
}

// routerConfig builds a fully specified router configuration for statePath.
func routerConfig(statePath string, startupDeadline time.Duration) routing.RouterConfig {
	return routing.RouterConfig{
		StatePath:       statePath,
		ProbeTimeout:    20 * time.Millisecond,
		ProbeInterval:   5 * time.Millisecond,
		StartupDeadline: startupDeadline,
		DrainTimeout:    50 * time.Millisecond,
	}
}

// newRouter constructs a router over statePath with test policies.
func newRouter(t *testing.T, statePath string) *routing.Router {
	t.Helper()

	router, err := routing.NewRouter(routerConfig(statePath, time.Second))
	if err != nil {
		t.Fatal(err)
	}

	return router
}

// newServer constructs a server over freshly opened listeners for
// construction-only tests; the router inside is freshly built too.
func newServer(t *testing.T, traffic, control net.Listener, statePath string, startupDeadline time.Duration) (*routing.Server, error) {
	t.Helper()

	router, err := routing.NewRouter(routerConfig(statePath, startupDeadline))
	if err != nil {
		return nil, err
	}

	return routing.NewServer(routing.ServerConfig{
		TrafficListener: traffic,
		ControlListener: control,
		Router:          router,
		ShutdownTimeout: time.Second,
	})
}

// serveServer constructs a router and server over the given listeners and
// runs it until ctx is canceled, returning its terminal error.
func serveServer(t *testing.T, traffic, control net.Listener, statePath string, startupDeadline time.Duration, ctx context.Context) error {
	t.Helper()

	router, err := routing.NewRouter(routerConfig(statePath, startupDeadline))
	if err != nil {
		return err
	}
	server, err := routing.NewServer(routing.ServerConfig{
		TrafficListener: traffic,
		ControlListener: control,
		Router:          router,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		return err
	}

	return server.Serve(ctx)
}

type runningRouting struct {
	traffic string
	client  sunbeampbconnect.SunbeamClient
	cancel  context.CancelFunc
	done    chan error
	stopped bool
}

func startRouting(t *testing.T, statePath string, startupDeadline time.Duration) *runningRouting {
	t.Helper()
	traffic := listen(t)
	control := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveServer(t, traffic, control, statePath, startupDeadline, ctx) }()
	waitForHTTP(t, "http://"+control.Addr().String()+"/sunstone.sunbeam.v1.Sunbeam/GetRoute")
	running := &runningRouting{
		traffic: "http://" + traffic.Addr().String(),
		client:  sunbeampbconnect.NewSunbeamClient(http.DefaultClient, "http://"+control.Addr().String()),
		cancel:  cancel,
		done:    done,
	}
	t.Cleanup(func() {
		if !running.stopped {
			running.stop(t)
		}
	})

	return running
}

func (r *runningRouting) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	r.stopped = true
}

func routeRequest(containerID, address string) *sunbeampb.UpdateRouteRequest {
	return &sunbeampb.UpdateRouteRequest{
		Route: &sunbeampb.Route{
			Name: "routes/storefront",
			Backend: &sunbeampb.Backend{
				Address:          address,
				ContainerId:      containerID,
				StartupProbePath: "/readyz",
			},
		},
		AllowMissing: true,
	}
}

func updateRoute(t *testing.T, client sunbeampbconnect.SunbeamClient, containerID, address string) {
	t.Helper()
	if _, err := client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest(containerID, address))); err != nil {
		t.Fatal(err)
	}
}

func getBody(t *testing.T, address string) string {
	t.Helper()
	response, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.StatusCode, body)
	}

	return string(body)
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return listener
}

func waitForHTTP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Post(address, "application/proto", nil)
		if err == nil {
			response.Body.Close()

			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server %s did not start", address)
}

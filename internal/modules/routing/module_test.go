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
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"github.com/goevery/sunstone/internal/modules/routing"
)

func TestRejectsNonLoopbackControlListener(t *testing.T) {
	traffic := listen(t)
	defer traffic.Close()
	control, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	_, err = routing.New(routing.Config{
		TrafficListener: traffic,
		ControlListener: control,
		StatePath:       filepath.Join(t.TempDir(), "routes.json"),
		ProbeTimeout:    time.Second,
		ProbeInterval:   time.Second,
		StartupDeadline: time.Second,
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected non-loopback control listener to be rejected")
	}
}

func TestRejectsMalformedPersistedState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(statePath, []byte(`{"version":1,"route":`), 0600); err != nil {
		t.Fatal(err)
	}
	traffic := listen(t)
	defer traffic.Close()
	control := listen(t)
	defer control.Close()

	_, err := routing.New(routing.Config{
		TrafficListener: traffic,
		ControlListener: control,
		StatePath:       statePath,
		ProbeTimeout:    time.Second,
		ProbeInterval:   time.Second,
		StartupDeadline: time.Second,
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
	})
	if err == nil {
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
	module, err := routing.New(routing.Config{
		TrafficListener: traffic,
		ControlListener: control,
		StatePath:       filepath.Join(t.TempDir(), "routes.json"),
		ProbeTimeout:    100 * time.Millisecond,
		ProbeInterval:   10 * time.Millisecond,
		StartupDeadline: time.Second,
		DrainTimeout:    time.Second,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- module.Serve(ctx) }()
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
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("update error = %v", err)
	}
	if body := getBody(t, running.traffic); body != "current" {
		t.Fatalf("proxied body after failed update = %q", body)
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
	module, err := routing.New(routing.Config{
		TrafficListener: traffic,
		ControlListener: control,
		StatePath:       statePath,
		ProbeTimeout:    20 * time.Millisecond,
		ProbeInterval:   5 * time.Millisecond,
		StartupDeadline: startupDeadline,
		DrainTimeout:    50 * time.Millisecond,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- module.Serve(ctx) }()
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

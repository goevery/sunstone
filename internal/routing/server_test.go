package routing_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

func TestProxyMatchesHostnamesExactly(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(writer, "storefront")
	}))
	defer backend.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example", "www.storefront.example"}, "container-1", backend.Listener.Addr().String())

	for _, authority := range []string{"storefront.example", "STOREFRONT.EXAMPLE:8080", "storefront.example.", "www.storefront.example:443"} {
		if body := getBody(t, running.traffic, authority); body != "storefront" {
			t.Fatalf("proxied body for %q = %q", authority, body)
		}
	}
	if status := getStatus(t, running.traffic, "other.example"); status != http.StatusNotFound {
		t.Fatalf("unrouted hostname status = %d", status)
	}
	if status := getStatus(t, running.traffic, "storefront.example:notaport"); status != http.StatusBadRequest {
		t.Fatalf("malformed authority status = %d", status)
	}
	if status := getStatus(t, running.traffic, "https://storefront.example"); status != http.StatusBadRequest {
		t.Fatalf("url authority status = %d", status)
	}
	if status := getStatus(t, running.traffic, "[::1]:8080"); status != http.StatusNotFound {
		t.Fatalf("bracketed literal status = %d", status)
	}
	if status := getRawStatus(t, running.traffic); status != http.StatusBadRequest {
		t.Fatalf("empty host status = %d", status)
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

	if status := getStatus(t, "http://"+traffic.Addr().String(), "storefront.example"); status != http.StatusNotFound {
		t.Fatalf("status before route = %d", status)
	}

	client := sunbeampbconnect.NewSunbeamClient(http.DefaultClient, "http://"+control.Addr().String())
	updated, err := client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("routes/storefront", []string{"storefront.example"}, "container-1", backend.Listener.Addr().String())))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Msg.GetBackend().GetContainerId() != "container-1" {
		t.Fatalf("updated route = %+v", updated.Msg)
	}
	got, err := client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"}))
	if err != nil || got.Msg.GetBackend().GetContainerId() != "container-1" || len(got.Msg.GetHostnames()) != 1 {
		t.Fatalf("get route = %+v, %v", got, err)
	}
	listed, err := client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{}))
	if err != nil || len(listed.Msg.GetRoutes()) != 1 {
		t.Fatalf("list routes = %+v, %v", listed, err)
	}

	proxyRequest, err := http.NewRequest(http.MethodPost, "http://"+traffic.Addr().String()+"/hello?from=test", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	proxyRequest.Host = "storefront.example"
	proxyRequest.Header.Set("X-Forwarded-For", "spoofed")
	response, err := http.DefaultClient.Do(proxyRequest)
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

func TestHostnameClaimsAreExclusive(t *testing.T) {
	first := readyBackend(t, "first")
	defer first.Close()
	second := readyBackend(t, "second")
	defer second.Close()
	third := readyBackend(t, "third")
	defer third.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", first.Listener.Addr().String())

	// Another workload may not claim an owned hostname...
	conflicting := routeRequest("routes/checkout", []string{"storefront.example"}, "container-2", second.Listener.Addr().String())
	conflicting.AllowMissing = true
	if _, err := running.client.UpdateRoute(context.Background(), connect.NewRequest(conflicting)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("conflicting hostname error = %v", err)
	}
	// ...but its own route may keep claiming it while changing backends.
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-3", third.Listener.Addr().String())

	// A different hostname belongs to a different workload.
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-2", second.Listener.Addr().String())
	if body := getBody(t, running.traffic, "storefront.example"); body != "third" {
		t.Fatalf("storefront body = %q", body)
	}
	if body := getBody(t, running.traffic, "checkout.example"); body != "second" {
		t.Fatalf("checkout body = %q", body)
	}

	route, err := running.client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"}))
	if err != nil || route.Msg.GetBackend().GetContainerId() != "container-3" {
		t.Fatalf("storefront route = %+v, %v", route.Msg, err)
	}
}

func TestConcurrentConflictingUpdatesDoNotStealHostnames(t *testing.T) {
	storefrontCurrent := readyBackend(t, "storefront-current")
	defer storefrontCurrent.Close()
	storefrontNext := readyBackend(t, "storefront-next")
	defer storefrontNext.Close()
	checkoutCurrent := readyBackend(t, "checkout-current")
	defer checkoutCurrent.Close()
	checkoutNext := readyBackend(t, "checkout-next")
	defer checkoutNext.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", storefrontCurrent.Listener.Addr().String())
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-2", checkoutCurrent.Listener.Addr().String())

	// Two workloads race for the same previously unclaimed hostname: the
	// first commit wins it, the loser keeps its previous routing, and
	// neither steals from the other.
	storefrontUpdate := updateAsync(running.client, routeRequest("routes/storefront", []string{"shared.example"}, "container-3", storefrontNext.Listener.Addr().String()))
	checkoutUpdate := updateAsync(running.client, routeRequest("routes/checkout", []string{"shared.example"}, "container-4", checkoutNext.Listener.Addr().String()))
	storefrontErr := <-storefrontUpdate
	checkoutErr := <-checkoutUpdate
	if (storefrontErr == nil) == (checkoutErr == nil) {
		t.Fatalf("conflicting updates = %v and %v; want exactly one failure", storefrontErr, checkoutErr)
	}
	var rejected error
	winnerBody := "checkout-next"
	loser, loserHostname, loserBody := "storefront", "storefront.example", "storefront-current"
	if storefrontErr == nil {
		winnerBody = "storefront-next"
		loser, loserHostname, loserBody = "checkout", "checkout.example", "checkout-current"
		rejected = checkoutErr
	} else {
		rejected = storefrontErr
	}
	if connect.CodeOf(rejected) != connect.CodeFailedPrecondition || !strings.Contains(rejected.Error(), "shared.example") {
		t.Fatalf("conflict error = %v", rejected)
	}

	if body := getBody(t, running.traffic, "shared.example"); body != winnerBody {
		t.Fatalf("shared hostname body = %q", body)
	}
	if body := getBody(t, running.traffic, loserHostname); body != loserBody {
		t.Fatalf("%s body after losing the race = %q", loser, body)
	}
}

func TestWorkloadMutationsStayIndependent(t *testing.T) {
	storefront := readyBackend(t, "storefront-current")
	defer storefront.Close()
	// The candidate never becomes ready, so the update probes for the
	// whole startup budget.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	checkoutCurrent := readyBackend(t, "checkout-current")
	defer checkoutCurrent.Close()
	checkoutNext := readyBackend(t, "checkout-next")
	defer checkoutNext.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), 500*time.Millisecond)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", storefront.Listener.Addr().String())
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-2", checkoutCurrent.Listener.Addr().String())

	storefrontUpdate := updateAsync(running.client, routeRequest("routes/storefront", []string{"storefront.example"}, "container-3", failing.Listener.Addr().String()))
	// While the first workload spends its startup budget, the second
	// workload deploys and serves.
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-4", checkoutNext.Listener.Addr().String())
	select {
	case err := <-storefrontUpdate:
		t.Fatalf("first workload's update ended before the second's: %v", err)
	default:
	}
	if body := getBody(t, running.traffic, "checkout.example"); body != "checkout-next" {
		t.Fatalf("checkout body during concurrent probe = %q", body)
	}
	if err := <-storefrontUpdate; connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("failing candidate error = %v", err)
	}
	if body := getBody(t, running.traffic, "storefront.example"); body != "storefront-current" {
		t.Fatalf("storefront body after failed update = %q", body)
	}
}

func TestDrainingWorkloadDoesNotBlockOthers(t *testing.T) {
	admitted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusNoContent)
		case "/hold":
			close(admitted)
			<-release
			_, _ = io.WriteString(w, "storefront-current")
		default:
			_, _ = io.WriteString(w, "storefront-current")
		}
	}))
	defer current.Close()
	replacement := readyBackend(t, "storefront-next")
	defer replacement.Close()
	checkout := readyBackend(t, "checkout")
	defer checkout.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", current.Listener.Addr().String())
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-2", checkout.Listener.Addr().String())

	oldResponse := make(chan string, 1)
	go func() { oldResponse <- getBody(t, running.traffic+"/hold", "storefront.example") }()
	<-admitted

	// The drain admits no new old-backend traffic, but the switch already
	// happened: new requests reach the replacement, and the unrelated
	// workload keeps serving while the drain waits.
	updated := updateAsync(running.client, routeRequest("routes/storefront", []string{"storefront.example"}, "container-3", replacement.Listener.Addr().String()))
	select {
	case err := <-updated:
		t.Fatalf("update completed before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// The switch is committed before the drain, so the replacement serves
	// while the old backend still waits on its admitted request.
	waitForBody(t, running.traffic, "storefront.example", "storefront-next")
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-4", checkout.Listener.Addr().String())
	if body := getBody(t, running.traffic, "checkout.example"); body != "checkout" {
		t.Fatalf("checkout body during foreign drain = %q", body)
	}

	releaseOnce.Do(func() { close(release) })
	if body := <-oldResponse; body != "storefront-current" {
		t.Fatalf("admitted response = %q", body)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSwitchesTheCompleteHostnameSet(t *testing.T) {
	backend := readyBackend(t, "storefront")
	defer backend.Close()
	statePath := filepath.Join(t.TempDir(), "routes.json")

	running := startRouting(t, statePath, time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"old.example"}, "container-1", backend.Listener.Addr().String())
	updateRoute(t, running.client, "routes/storefront", []string{"new.example", "newer.example"}, "container-2", backend.Listener.Addr().String())

	if status := getStatus(t, running.traffic, "old.example"); status != http.StatusNotFound {
		t.Fatalf("released hostname status = %d", status)
	}
	if body := getBody(t, running.traffic, "new.example"); body != "storefront" {
		t.Fatalf("new hostname body = %q", body)
	}
	if body := getBody(t, running.traffic, "newer.example"); body != "storefront" {
		t.Fatalf("newer hostname body = %q", body)
	}
	route, err := running.client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"}))
	if err != nil || len(route.Msg.GetHostnames()) != 2 || route.Msg.GetHostnames()[0] != "new.example" {
		t.Fatalf("route after hostname switch = %+v, %v", route.Msg, err)
	}

	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "new.example") || strings.Contains(string(state), "old.example") {
		t.Fatalf("persisted state = %s", state)
	}
}

func TestFailedCandidateLeavesCurrentRouteActive(t *testing.T) {
	current := readyBackend(t, "current")
	defer current.Close()
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failed.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), 50*time.Millisecond)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-current", current.Listener.Addr().String())
	_, err := running.client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("routes/storefront", []string{"storefront.example"}, "container-failed", failed.Listener.Addr().String())))
	// The startup probe exhausted its own deadline before any route
	// change: the rejection is definitive and identifies itself, so the
	// caller can safely clean up the failed candidate.
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("update error = %v", err)
	}
	if !strings.Contains(err.Error(), "backend failed startup probe") || !strings.Contains(err.Error(), "startup probe deadline exceeded") {
		t.Fatalf("update error lost its cause: %v", err)
	}
	if body := getBody(t, running.traffic, "storefront.example"); body != "current" {
		t.Fatalf("proxied body after failed update = %q", body)
	}
}

func TestCanceledStartupProbeStaysUnconfirmed(t *testing.T) {
	// Canceling the update's caller mid-probe proves nothing about the
	// candidate: the answer stays an ordinary cancellation instead of the
	// definitive startup-deadline rejection, and the current route keeps
	// serving.
	current := readyBackend(t, "current")
	defer current.Close()
	probed := make(chan struct{})
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probed)
		<-r.Context().Done()
	}))
	defer candidate.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), 5*time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-current", current.Listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	update := make(chan error, 1)
	go func() {
		_, err := running.client.UpdateRoute(ctx, connect.NewRequest(routeRequest("routes/storefront", []string{"storefront.example"}, "container-new", candidate.Listener.Addr().String())))
		update <- err
	}()
	<-probed
	cancel()
	if err := <-update; connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("update error = %v", err)
	}
	if body := getBody(t, running.traffic, "storefront.example"); body != "current" {
		t.Fatalf("proxied body after canceled update = %q", body)
	}
}

func TestSameWorkloadMutationsAreSerialized(t *testing.T) {
	current := readyBackend(t, "current")
	defer current.Close()
	probed := make(chan struct{})
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probed)
		<-r.Context().Done()
	}))
	defer candidate.Close()
	statePath := filepath.Join(t.TempDir(), "routes.json")

	router, err := routing.NewRouter(routerConfig(statePath, 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	route := routing.Route{
		Name:      "routes/storefront",
		Hostnames: []string{"storefront.example"},
		Backend:   routing.Backend{Address: current.Listener.Addr().String(), ContainerID: "container-1", StartupProbePath: "/readyz"},
	}
	if _, err := router.UpdateRoute(context.Background(), route, nil, true); err != nil {
		t.Fatal(err)
	}

	candidateRoute := route
	candidateRoute.Backend = routing.Backend{Address: candidate.Listener.Addr().String(), ContainerID: "container-2", StartupProbePath: "/readyz"}
	updateCtx, cancelUpdate := context.WithCancel(context.Background())
	update := make(chan error, 1)
	go func() {
		_, err := router.UpdateRoute(updateCtx, candidateRoute, nil, false)
		update <- err
	}()
	<-probed

	// A deletion of the same workload waits for the in-flight mutation
	// instead of interleaving with the probe.
	deleted := make(chan error, 1)
	go func() { deleted <- router.DeleteRoute("routes/storefront", false) }()
	select {
	case err := <-deleted:
		t.Fatalf("delete finished while the update was probing: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	cancelUpdate()
	if err := <-update; err == nil {
		t.Fatal("expected the canceled update to fail")
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete after serialized update: %v", err)
	}
	if _, err := router.GetRoute("routes/storefront"); !errors.Is(err, routing.ErrNotFound) {
		t.Fatalf("route after deletion = %v", err)
	}
}

func TestFailedPersistencePreservesCurrentRoute(t *testing.T) {
	current := readyBackend(t, "current")
	defer current.Close()
	replacement := readyBackend(t, "replacement")
	defer replacement.Close()
	statePath := filepath.Join(t.TempDir(), "routes.json")

	// The router is exercised directly: a control-server restart is not
	// part of the scenario, and routing construction itself must stay
	// possible because the state file only becomes unreplaceable after the
	// first route is in place.
	first, err := routing.NewRouter(routerConfig(statePath, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	route := routing.Route{
		Name:      "routes/storefront",
		Hostnames: []string{"storefront.example"},
		Backend:   routing.Backend{Address: current.Listener.Addr().String(), ContainerID: "container-current", StartupProbePath: "/readyz"},
	}
	if _, err := first.UpdateRoute(context.Background(), route, nil, true); err != nil {
		t.Fatal(err)
	}

	// A state path that can no longer be replaced makes the switch fail
	// after the candidate is ready; the previous routing must survive
	// intact, including admission to the previous backend.
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	candidate := route
	candidate.Backend = routing.Backend{Address: replacement.Listener.Addr().String(), ContainerID: "container-new", StartupProbePath: "/readyz"}
	if _, err := first.UpdateRoute(context.Background(), candidate, nil, false); err == nil {
		t.Fatal("expected persistence failure")
	} else if errors.Is(err, routing.ErrFailedPrecondition) {
		t.Fatalf("persistence failure misclassified: %v", err)
	}

	got, err := first.GetRoute("routes/storefront")
	if err != nil || got.Backend.ContainerID != "container-current" {
		t.Fatalf("route after failed persistence = %+v, %v", got, err)
	}
	recorder := httptest.NewRecorder()
	proxyRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	proxyRequest.Host = "storefront.example"
	first.Proxy(recorder, proxyRequest)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "current" {
		t.Fatalf("proxied response after failed persistence = %d %q", recorder.Code, recorder.Body.String())
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
	replacement := readyBackend(t, "replacement")
	defer replacement.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-current", current.Listener.Addr().String())
	oldResponse := make(chan string, 1)
	go func() { oldResponse <- getBody(t, running.traffic+"/hold", "storefront.example") }()
	<-admitted

	updated := make(chan error, 1)
	go func() {
		_, err := running.client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest("routes/storefront", []string{"storefront.example"}, "container-new", replacement.Listener.Addr().String())))
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
	if body := getBody(t, running.traffic, "storefront.example"); body != "replacement" {
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
	replacement := readyBackend(t, "replacement")
	defer replacement.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-current", current.Listener.Addr().String())
	requestDone := make(chan error, 1)
	go func() {
		if status, _ := request(t, running.traffic, "storefront.example"); status != http.StatusOK {
			requestDone <- fmt.Errorf("stuck request was not admitted: %d", status)
			return
		}
		requestDone <- nil
	}()
	<-admitted
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-new", replacement.Listener.Addr().String())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stuck backend request was not canceled")
	}
	<-requestDone
	if body := getBody(t, running.traffic, "storefront.example"); body != "replacement" {
		t.Fatalf("new response = %q", body)
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
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", backend.Listener.Addr().String())
	response, err := http.NewRequest(http.MethodGet, running.traffic+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Host = "storefront.example"
	stream, err := http.DefaultClient.Do(response)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "first\n" {
		t.Fatalf("first streamed chunk = %q, %v", first, err)
	}
	close(releaseStream)
	second, err := reader.ReadString('\n')
	stream.Body.Close()
	if err != nil || second != "second\n" {
		t.Fatalf("second streamed chunk = %q, %v", second, err)
	}

	websocketURL := "ws" + strings.TrimPrefix(running.traffic, "http") + "/socket"
	connection, _, err := websocket.Dial(context.Background(), websocketURL, &websocket.DialOptions{Host: "storefront.example"})
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

func TestDeleteRouteDrainsOnlyItsOwnWorkload(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "routes.json")
	admitted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	draining := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/readyz":
			writer.WriteHeader(http.StatusNoContent)
		case "/hold":
			close(admitted)
			<-release
			_, _ = io.WriteString(writer, "storefront")
		default:
			_, _ = io.WriteString(writer, "storefront")
		}
	}))
	defer draining.Close()
	other := readyBackend(t, "checkout")
	defer other.Close()

	running := startRouting(t, statePath, time.Second)
	updateRoute(t, running.client, "routes/storefront", []string{"storefront.example"}, "container-1", draining.Listener.Addr().String())
	updateRoute(t, running.client, "routes/checkout", []string{"checkout.example"}, "container-2", other.Listener.Addr().String())

	requestDone := make(chan error, 1)
	go func() {
		hold, err := http.NewRequest(http.MethodGet, running.traffic+"/hold", nil)
		if err != nil {
			requestDone <- err
			return
		}
		hold.Host = "storefront.example"
		response, err := http.DefaultClient.Do(hold)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		requestDone <- err
	}()
	<-admitted

	deleteDone := make(chan error, 1)
	go func() {
		_, err := running.client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront"}))
		deleteDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if status := getStatus(t, running.traffic, "storefront.example"); status == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted hostname kept accepting traffic")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-deleteDone:
		t.Fatalf("delete returned before admitted request drained: %v", err)
	default:
	}
	// The surviving workload is untouched throughout the drain.
	if body := getBody(t, running.traffic, "checkout.example"); body != "checkout" {
		t.Fatalf("checkout body during foreign drain = %q", body)
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if _, err := running.client.GetRoute(context.Background(), connect.NewRequest(&sunbeampb.GetRouteRequest{Name: "routes/storefront"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("deleted route error = %v", err)
	}
	if _, err := running.client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing route error = %v", err)
	}
	if _, err := running.client.DeleteRoute(context.Background(), connect.NewRequest(&sunbeampb.DeleteRouteRequest{Name: "routes/storefront", AllowMissing: true})); err != nil {
		t.Fatalf("allow missing delete: %v", err)
	}
	// The deletion is durable: only the surviving route remains in the
	// persisted table.
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Routes []struct {
			Name      string   `json:"name"`
			Hostnames []string `json:"hostnames"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(state, &document); err != nil || len(document.Routes) != 1 ||
		document.Routes[0].Name != "routes/checkout" ||
		!slices.Equal(document.Routes[0].Hostnames, []string{"checkout.example"}) {
		t.Fatalf("persisted state after deletion = %s (%v)", state, err)
	}
	if body := getBody(t, running.traffic, "checkout.example"); body != "checkout" {
		t.Fatalf("checkout body after foreign deletion = %q", body)
	}
}

func TestRestoresPersistedRoutes(t *testing.T) {
	storefront := readyBackend(t, "storefront")
	defer storefront.Close()
	checkout := readyBackend(t, "checkout")
	defer checkout.Close()
	statePath := filepath.Join(t.TempDir(), "routes.json")

	first := startRouting(t, statePath, time.Second)
	updateRoute(t, first.client, "routes/storefront", []string{"storefront.example"}, "container-1", storefront.Listener.Addr().String())
	updateRoute(t, first.client, "routes/checkout", []string{"checkout.example"}, "container-2", checkout.Listener.Addr().String())
	first.stop(t)

	second := startRouting(t, statePath, time.Second)
	defer second.stop(t)
	if body := getBody(t, second.traffic, "storefront.example"); body != "storefront" {
		t.Fatalf("restored storefront body = %q", body)
	}
	if body := getBody(t, second.traffic, "checkout.example"); body != "checkout" {
		t.Fatalf("restored checkout body = %q", body)
	}
	// Updates after a reload keep coordinating the restored claims.
	conflicting := routeRequest("routes/other", []string{"checkout.example"}, "container-3", checkout.Listener.Addr().String())
	conflicting.AllowMissing = true
	if _, err := second.client.UpdateRoute(context.Background(), connect.NewRequest(conflicting)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("restored claim conflict error = %v", err)
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

func TestRejectsInconsistentPersistedRoutes(t *testing.T) {
	route := func(name, hostname string) string {
		return `{"name":"` + name + `","hostnames":["` + hostname + `"],"backend":{"address":"storefront-01234567:8080","containerId":"container-1","startupProbePath":"/readyz"}}`
	}
	tests := map[string]string{
		"shared hostname": `{"version":1,"routes":[` + route("routes/storefront", "shared.example") + `,` + route("routes/checkout", "shared.example") + `]}`,
		"repeated route":  `{"version":1,"routes":[` + route("routes/storefront", "storefront.example") + `,` + route("routes/storefront", "other.example") + `]}`,
		"invalid route":   `{"version":1,"routes":[` + route("routes/storefront", "Storefront.example") + `]}`,
	}
	for name, state := range tests {
		t.Run(name, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "routes.json")
			if err := os.WriteFile(statePath, []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := routing.NewRouter(routerConfig(statePath, time.Second)); err == nil {
				t.Fatal("expected inconsistent state to fail construction")
			}
		})
	}
}

func TestListRoutesPaginatesByName(t *testing.T) {
	storefront := readyBackend(t, "storefront")
	defer storefront.Close()

	running := startRouting(t, filepath.Join(t.TempDir(), "routes.json"), time.Second)
	updateRoute(t, running.client, "routes/gamma", []string{"gamma.example"}, "container-3", storefront.Listener.Addr().String())
	updateRoute(t, running.client, "routes/alpha", []string{"alpha.example"}, "container-1", storefront.Listener.Addr().String())
	updateRoute(t, running.client, "routes/beta", []string{"beta.example"}, "container-2", storefront.Listener.Addr().String())

	firstPage, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{PageSize: 2}))
	if err != nil || len(firstPage.Msg.GetRoutes()) != 2 ||
		firstPage.Msg.GetRoutes()[0].GetName() != "routes/alpha" ||
		firstPage.Msg.GetRoutes()[1].GetName() != "routes/beta" {
		t.Fatalf("first page = %+v, %v", firstPage.Msg, err)
	}
	if token := firstPage.Msg.GetNextPageToken(); token == "" || token == "routes/beta" {
		t.Fatalf("next page token = %q; want an opaque token", token)
	}
	secondPage, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{PageSize: 2, PageToken: firstPage.Msg.GetNextPageToken()}))
	if err != nil || len(secondPage.Msg.GetRoutes()) != 1 || secondPage.Msg.GetRoutes()[0].GetName() != "routes/gamma" || secondPage.Msg.GetNextPageToken() != "" {
		t.Fatalf("second page = %+v, %v", secondPage.Msg, err)
	}

	defaultPage, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{}))
	if err != nil || len(defaultPage.Msg.GetRoutes()) != 3 {
		t.Fatalf("default page = %+v, %v", defaultPage.Msg, err)
	}
	if _, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{PageSize: -1})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative page size error = %v", err)
	}
	if _, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{PageToken: "not-a-token"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid page token error = %v", err)
	}
	decoded := base64.RawURLEncoding.EncodeToString([]byte("garbage"))
	if _, err := running.client.ListRoutes(context.Background(), connect.NewRequest(&sunbeampb.ListRoutesRequest{PageToken: decoded})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("decoded-but-invalid page token error = %v", err)
	}
}

func TestRouteHostnamesAreOwnedAtBoundaries(t *testing.T) {
	backend := readyBackend(t, "storefront")
	defer backend.Close()

	router, err := routing.NewRouter(routerConfig(filepath.Join(t.TempDir(), "routes.json"), time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	// The caller keeps and later mutates the slice it passed in.
	hostnames := []string{"storefront.example"}
	route := routing.Route{
		Name:      "routes/storefront",
		Hostnames: hostnames,
		Backend:   routing.Backend{Address: backend.Listener.Addr().String(), ContainerID: "container-1", StartupProbePath: "/readyz"},
	}
	if _, err := router.UpdateRoute(context.Background(), route, nil, true); err != nil {
		t.Fatal(err)
	}
	hostnames[0] = "corrupted.example"
	stored, err := router.GetRoute("routes/storefront")
	if err != nil || stored.Hostnames[0] != "storefront.example" {
		t.Fatalf("route after caller mutated its input = %+v, %v", stored, err)
	}

	// The reader keeps and later mutates the slice it got back.
	stored.Hostnames[0] = "mutated.example"
	again, err := router.GetRoute("routes/storefront")
	if err != nil || again.Hostnames[0] != "storefront.example" {
		t.Fatalf("route after reader mutated its result = %+v, %v", again, err)
	}
	listed, _, err := router.ListRoutes(0, "")
	if err != nil || len(listed) != 1 || listed[0].Hostnames[0] != "storefront.example" {
		t.Fatalf("listed routes = %+v, %v", listed, err)
	}
	listed[0].Hostnames[0] = "mutated.example"

	// An update result belongs to its caller too: mutating the returned
	// hostnames leaves committed state alone.
	updated, err := router.UpdateRoute(context.Background(), routing.Route{
		Name:      "routes/storefront",
		Hostnames: []string{"storefront.example", "www.storefront.example"},
		Backend:   routing.Backend{Address: backend.Listener.Addr().String(), ContainerID: "container-2", StartupProbePath: "/readyz"},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	updated.Hostnames[0] = "corrupted.example"
	confirmed, err := router.GetRoute("routes/storefront")
	if err != nil || !slices.Equal(confirmed.Hostnames, []string{"storefront.example", "www.storefront.example"}) {
		t.Fatalf("route after caller mutated its update result = %+v, %v", confirmed, err)
	}

	if status := getStatus(t, routerAddress(t, router), "corrupted.example"); status != http.StatusNotFound {
		t.Fatalf("corrupted hostname status = %d", status)
	}
	if status := getStatus(t, routerAddress(t, router), "mutated.example"); status != http.StatusNotFound {
		t.Fatalf("mutated hostname status = %d", status)
	}
	if status := getStatus(t, routerAddress(t, router), "storefront.example"); status != http.StatusOK {
		t.Fatalf("storefront hostname status = %d", status)
	}
}

// routerAddress exposes a direct proxy handle for router-level tests.
func routerAddress(t *testing.T, router *routing.Router) string {
	t.Helper()
	listener := listen(t)
	go func() {
		server := &http.Server{Handler: http.HandlerFunc(router.Proxy)}
		defer server.Close()
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { listener.Close() })
	waitForHTTP(t, "http://"+listener.Addr().String()+"/probe")

	return "http://" + listener.Addr().String()
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
	traffic   string
	statePath string
	client    sunbeampbconnect.SunbeamClient
	cancel    context.CancelFunc
	done      chan error
	stopped   bool
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
		traffic:   "http://" + traffic.Addr().String(),
		statePath: statePath,
		client:    sunbeampbconnect.NewSunbeamClient(http.DefaultClient, "http://"+control.Addr().String()),
		cancel:    cancel,
		done:      done,
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

// readyBackend starts a backend that answers /readyz and echoes body.
func readyBackend(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
}

// updateAsync runs one route update in the background.
func updateAsync(client sunbeampbconnect.SunbeamClient, request *sunbeampb.UpdateRouteRequest) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := client.UpdateRoute(context.Background(), connect.NewRequest(request))
		result <- err
	}()

	return result
}

func routeRequest(name string, hostnames []string, containerID, address string) *sunbeampb.UpdateRouteRequest {
	return &sunbeampb.UpdateRouteRequest{
		Route: &sunbeampb.Route{
			Name:      name,
			Hostnames: hostnames,
			Backend: &sunbeampb.Backend{
				Address:          address,
				ContainerId:      containerID,
				StartupProbePath: "/readyz",
			},
		},
		AllowMissing: true,
	}
}

func updateRoute(t *testing.T, client sunbeampbconnect.SunbeamClient, name string, hostnames []string, containerID, address string) {
	t.Helper()
	if _, err := client.UpdateRoute(context.Background(), connect.NewRequest(routeRequest(name, hostnames, containerID, address))); err != nil {
		t.Fatal(err)
	}
}

// getBody fetches one hostname-routed path and requires a 200 response.
func getBody(t *testing.T, address, authority string) string {
	t.Helper()
	status, body := request(t, address, authority)
	if status != http.StatusOK {
		t.Fatalf("status for %q = %d, body = %q", authority, status, body)
	}

	return body
}

// getStatus fetches one hostname-routed path and returns its status.
func getStatus(t *testing.T, address, authority string) int {
	t.Helper()
	status, _ := request(t, address, authority)

	return status
}

// waitForBody polls one hostname-routed path until it serves want.
func waitForBody(t *testing.T, address, authority, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, body := request(t, address, authority); body == want {
			return
		}
		if time.Now().After(deadline) {
			_, body := request(t, address, authority)
			t.Fatalf("body for %q = %q, want %q", authority, body, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func request(t *testing.T, address, authority string) (int, string) {
	t.Helper()
	probe, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	probe.Host = authority
	response, err := http.DefaultClient.Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return response.StatusCode, string(body)
}

// getRawStatus sends one request without any Host header.
func getRawStatus(t *testing.T, address string) int {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("GET / HTTP/1.1\r\nHost:\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode
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
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Post(address, "application/proto", nil)
		if err == nil {
			response.Body.Close()

			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server %s did not start", address)
}

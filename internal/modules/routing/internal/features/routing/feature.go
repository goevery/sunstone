package routing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
)

const stateVersion = 1

// Config contains all required routing collaborators and policies.
type Config struct {
	TrafficListener net.Listener
	ControlListener net.Listener
	StatePath       string
	ProbeTimeout    time.Duration
	ProbeInterval   time.Duration
	StartupDeadline time.Duration
	DrainTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// Feature owns public proxying, route control, persistence, and draining.
type Feature struct {
	sunbeampbconnect.UnimplementedSunbeamHandler
	config Config

	active atomic.Pointer[backend]
	mu     sync.Mutex
	route  *sunbeampb.Route
}

// New restores durable state and constructs the routing feature.
func New(config Config) (*Feature, error) {
	feature := &Feature{config: config}
	route, err := loadState(config.StatePath)
	if err != nil {
		return nil, err
	}
	if route != nil {
		if err := validateRoute(route); err != nil {
			return nil, fmt.Errorf("validate persisted route: %w", err)
		}
		feature.route = cloneRoute(route)
		feature.active.Store(newBackend(route))
	}

	return feature, nil
}

// Serve runs the traffic and control servers until cancellation or failure.
func (f *Feature) Serve(ctx context.Context) error {
	trafficServer := &http.Server{Handler: http.HandlerFunc(f.proxy)}
	path, controlHandler := sunbeampbconnect.NewSunbeamHandler(f)
	controlMux := http.NewServeMux()
	controlMux.Handle(path, controlHandler)
	controlServer := &http.Server{Handler: controlMux}

	errorsCh := make(chan error, 2)
	go func() { errorsCh <- serve(trafficServer, f.config.TrafficListener) }()
	go func() { errorsCh <- serve(controlServer, f.config.ControlListener) }()

	var terminal error
	select {
	case <-ctx.Done():
	case terminal = <-errorsCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), f.config.ShutdownTimeout)
	defer cancel()
	shutdownErr := errors.Join(controlServer.Shutdown(shutdownCtx), trafficServer.Shutdown(shutdownCtx))
	if current := f.active.Load(); current != nil {
		current.stop()
	}
	if terminal != nil {
		return terminal
	}
	return shutdownErr
}

// GetRoute gets the configured route.
func (f *Feature) GetRoute(_ context.Context, request *connect.Request[sunbeampb.GetRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil || f.route.GetName() != request.Msg.GetName() {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("route not found"))
	}
	return connect.NewResponse(cloneRoute(f.route)), nil
}

// ListRoutes lists the configured route.
func (f *Feature) ListRoutes(_ context.Context, request *connect.Request[sunbeampb.ListRoutesRequest]) (*connect.Response[sunbeampb.ListRoutesResponse], error) {
	if request.Msg.GetPageSize() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	}
	if request.Msg.GetPageToken() != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_token is invalid"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	response := &sunbeampb.ListRoutesResponse{}
	if f.route != nil {
		response.Routes = []*sunbeampb.Route{cloneRoute(f.route)}
	}
	return connect.NewResponse(response), nil
}

// UpdateRoute validates, starts, persists, switches, and drains a route.
func (f *Feature) UpdateRoute(ctx context.Context, request *connect.Request[sunbeampb.UpdateRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	requested := request.Msg.GetRoute()
	if err := validateRouteName(requested); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	paths := request.Msg.GetUpdateMask().GetPaths()
	if err := validateMask(paths); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil && !request.Msg.GetAllowMissing() {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("route not found"))
	}
	if f.route != nil && f.route.GetName() != requested.GetName() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only one catch-all route is supported"))
	}
	candidate := applyUpdateMask(f.route, requested, paths)
	if err := validateRoute(candidate); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if reflect.DeepEqual(f.route, candidate) {
		return connect.NewResponse(cloneRoute(f.route)), nil
	}

	if err := f.waitUntilStarted(ctx, candidate.GetBackend()); err != nil {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	if err := persistState(f.config.StatePath, candidate); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("persist route: %w", err))
	}

	next := newBackend(candidate)
	previous := f.active.Swap(next)
	f.route = cloneRoute(candidate)
	if previous != nil {
		previous.drain(f.config.DrainTimeout)
	}

	return connect.NewResponse(cloneRoute(f.route)), nil
}

func (f *Feature) proxy(writer http.ResponseWriter, request *http.Request) {
	for {
		current := f.active.Load()
		if current == nil {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if current.acquire() {
			current.serve(writer, request)
			current.release()
			return
		}
	}
}

func (f *Feature) waitUntilStarted(ctx context.Context, candidate *sunbeampb.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, f.config.StartupDeadline)
	defer cancel()
	client := &http.Client{
		Timeout: f.config.ProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	probeURL := "http://" + candidate.GetAddress() + candidate.GetStartupProbePath()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 400 {
				return nil
			}
		}

		timer := time.NewTimer(f.config.ProbeInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("startup probe: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func serve(server *http.Server, listener net.Listener) error {
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Package routing runs Sunbeam's public proxy and private control
// interface: readiness-gated route switching, draining, durable route
// state, the HTTP proxy itself, and the loopback control transport.
package routing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultControlAddress is Sunbeam's control endpoint on the VM host.
	DefaultControlAddress = "127.0.0.1:2025"
	// DefaultStatePath is the route state location intended for a Docker volume.
	DefaultStatePath = "/var/lib/sunbeam/routes.json"
)

// Route control sentinel errors.
var (
	// ErrNotFound indicates that a requested route does not exist.
	ErrNotFound = errors.New("route not found")
	// ErrFailedPrecondition indicates that another catch-all route already exists.
	ErrFailedPrecondition = errors.New("only one catch-all route is supported")
	// ErrInvalidArgument indicates invalid route-control input.
	ErrInvalidArgument = errors.New("invalid route request")
	// ErrBackendNotReady indicates that a candidate backend failed its
	// startup probe before any route change was applied, so the previous
	// route keeps serving.
	ErrBackendNotReady = errors.New("backend failed startup probe")
)

// Route describes the active catch-all route.
type Route struct {
	Name    string  `json:"name"`
	Backend Backend `json:"backend"`
}

// Backend identifies one managed container endpoint.
type Backend struct {
	Address          string `json:"address"`
	ContainerID      string `json:"containerId"`
	StartupProbePath string `json:"startupProbePath"`
}

// RouterConfig contains the durable state location and bounded routing
// policies. All fields are mandatory.
type RouterConfig struct {
	StatePath       string
	ProbeTimeout    time.Duration
	ProbeInterval   time.Duration
	StartupDeadline time.Duration
	DrainTimeout    time.Duration
}

// Router owns public proxying, route state, startup checks, and draining.
type Router struct {
	config RouterConfig

	active atomic.Pointer[backend]
	mu     sync.Mutex
	route  *Route
}

// NewRouter restores durable state and constructs the router. A malformed
// persisted route fails construction; a restored route becomes active
// immediately.
func NewRouter(config RouterConfig) (*Router, error) {
	if config.StatePath == "" {
		return nil, errors.New("state path is required")
	}
	if config.ProbeTimeout <= 0 || config.ProbeInterval <= 0 || config.StartupDeadline <= 0 || config.DrainTimeout <= 0 {
		return nil, errors.New("routing durations must be positive")
	}

	router := &Router{config: config}
	route, err := loadState(config.StatePath)
	if err != nil {
		return nil, err
	}
	if route != nil {
		if err := validateRoute(*route); err != nil {
			return nil, fmt.Errorf("validate persisted route: %w", err)
		}
		stored := *route
		router.route = &stored
		router.active.Store(newBackend(stored))
	}

	return router, nil
}

// Proxy forwards one public HTTP request to the active backend.
func (r *Router) Proxy(writer http.ResponseWriter, request *http.Request) {
	for {
		current := r.active.Load()
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

// GetRoute returns the named route.
func (r *Router) GetRoute(name string) (Route, error) {
	if err := validateRouteName(name); err != nil {
		return Route{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.route == nil || r.route.Name != name {
		return Route{}, ErrNotFound
	}

	return *r.route, nil
}

// ListRoutes returns the configured route.
func (r *Router) ListRoutes(pageSize int32, pageToken string) ([]Route, error) {
	if pageSize < 0 {
		return nil, fmt.Errorf("%w: page_size must not be negative", ErrInvalidArgument)
	}
	if pageToken != "" {
		return nil, fmt.Errorf("%w: page_token is invalid", ErrInvalidArgument)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.route == nil {
		return nil, nil
	}

	return []Route{*r.route}, nil
}

// UpdateRoute validates, starts, persists, switches, and drains a route.
func (r *Router) UpdateRoute(ctx context.Context, requested Route, paths []string, allowMissing bool) (Route, error) {
	if err := validateRouteName(requested.Name); err != nil {
		return Route{}, err
	}
	if err := validateMask(paths); err != nil {
		return Route{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.route == nil && !allowMissing {
		return Route{}, ErrNotFound
	}
	if r.route != nil && r.route.Name != requested.Name {
		return Route{}, ErrFailedPrecondition
	}
	candidate := applyUpdateMask(r.route, requested, paths)
	if err := validateRoute(candidate); err != nil {
		return Route{}, err
	}
	if r.route != nil && *r.route == candidate {
		return *r.route, nil
	}

	if err := r.waitUntilStarted(ctx, candidate.Backend); err != nil {
		// A startup probe that exhausted its own deadline is a definitive
		// pre-switch rejection: the candidate never became ready, and the
		// route was neither persisted nor switched. A caller cancellation,
		// by contrast, stays an ordinary context failure.
		if errors.Is(err, errStartupDeadline) {
			return Route{}, fmt.Errorf("%w: %w", ErrBackendNotReady, err)
		}

		return Route{}, fmt.Errorf("startup probe: %w", err)
	}
	if err := persistState(r.config.StatePath, candidate); err != nil {
		return Route{}, fmt.Errorf("persist route: %w", err)
	}

	next := newBackend(candidate)
	previous := r.active.Load()
	if previous == nil {
		r.active.Store(next)
	} else {
		previous.drain(r.config.DrainTimeout, func() { r.active.Store(next) })
	}
	r.route = &candidate

	return candidate, nil
}

// DeleteRoute durably removes a route, stops new traffic, and drains
// admitted requests.
func (r *Router) DeleteRoute(name string, allowMissing bool) error {
	if err := validateRouteName(name); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.route == nil || r.route.Name != name {
		if allowMissing {
			return nil
		}

		return ErrNotFound
	}
	if err := removeState(r.config.StatePath); err != nil {
		return fmt.Errorf("persist route deletion: %w", err)
	}

	previous := r.active.Load()
	if previous != nil {
		previous.drain(r.config.DrainTimeout, func() { r.active.Store(nil) })
	} else {
		r.active.Store(nil)
	}
	r.route = nil

	return nil
}

// Close cancels active backend work and releases idle connections.
func (r *Router) Close() {
	if current := r.active.Load(); current != nil {
		current.stop()
	}
}

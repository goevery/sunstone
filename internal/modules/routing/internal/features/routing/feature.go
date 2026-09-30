package routing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const stateVersion = 1

// Config contains durable state and bounded operation policies.
type Config struct {
	StatePath       string
	ProbeTimeout    time.Duration
	ProbeInterval   time.Duration
	StartupDeadline time.Duration
	DrainTimeout    time.Duration
}

// Feature owns public proxying, route state, startup checks, and draining.
type Feature struct {
	config Config

	active atomic.Pointer[backend]
	mu     sync.Mutex
	route  *Route
}

// New restores durable state and constructs the routing feature.
func New(config Config) (*Feature, error) {
	feature := &Feature{config: config}
	route, err := loadState(config.StatePath)
	if err != nil {
		return nil, err
	}
	if route != nil {
		if err := validateRoute(*route); err != nil {
			return nil, fmt.Errorf("validate persisted route: %w", err)
		}
		stored := *route
		feature.route = &stored
		feature.active.Store(newBackend(stored))
	}

	return feature, nil
}

// Proxy forwards one public HTTP request to the active backend.
func (f *Feature) Proxy(writer http.ResponseWriter, request *http.Request) {
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

// GetRoute returns the named route.
func (f *Feature) GetRoute(name string) (Route, error) {
	if err := validateRouteName(name); err != nil {
		return Route{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil || f.route.Name != name {
		return Route{}, ErrNotFound
	}
	return *f.route, nil
}

// ListRoutes returns the configured route.
func (f *Feature) ListRoutes(pageSize int32, pageToken string) ([]Route, error) {
	if pageSize < 0 {
		return nil, fmt.Errorf("%w: page_size must not be negative", ErrInvalidArgument)
	}
	if pageToken != "" {
		return nil, fmt.Errorf("%w: page_token is invalid", ErrInvalidArgument)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil {
		return nil, nil
	}
	return []Route{*f.route}, nil
}

// UpdateRoute validates, starts, persists, switches, and drains a route.
func (f *Feature) UpdateRoute(ctx context.Context, requested Route, paths []string, allowMissing bool) (Route, error) {
	if err := validateRouteName(requested.Name); err != nil {
		return Route{}, err
	}
	if err := validateMask(paths); err != nil {
		return Route{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil && !allowMissing {
		return Route{}, ErrNotFound
	}
	if f.route != nil && f.route.Name != requested.Name {
		return Route{}, ErrFailedPrecondition
	}
	candidate := applyUpdateMask(f.route, requested, paths)
	if err := validateRoute(candidate); err != nil {
		return Route{}, err
	}
	if f.route != nil && *f.route == candidate {
		return *f.route, nil
	}

	if err := f.waitUntilStarted(ctx, candidate.Backend); err != nil {
		return Route{}, fmt.Errorf("startup probe: %w", err)
	}
	if err := persistState(f.config.StatePath, candidate); err != nil {
		return Route{}, fmt.Errorf("persist route: %w", err)
	}

	next := newBackend(candidate)
	previous := f.active.Load()
	if previous == nil {
		f.active.Store(next)
	} else {
		previous.drain(f.config.DrainTimeout, func() { f.active.Store(next) })
	}
	f.route = &candidate

	return candidate, nil
}

// DeleteRoute durably removes a route, stops new traffic, and drains admitted requests.
func (f *Feature) DeleteRoute(name string, allowMissing bool) error {
	if err := validateRouteName(name); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.route == nil || f.route.Name != name {
		if allowMissing {
			return nil
		}
		return ErrNotFound
	}
	if err := removeState(f.config.StatePath); err != nil {
		return fmt.Errorf("persist route deletion: %w", err)
	}

	previous := f.active.Load()
	if previous != nil {
		previous.drain(f.config.DrainTimeout, func() { f.active.Store(nil) })
	} else {
		f.active.Store(nil)
	}
	f.route = nil
	return nil
}

// Close cancels active backend work and releases idle connections.
func (f *Feature) Close() {
	if current := f.active.Load(); current != nil {
		current.stop()
	}
}

func (f *Feature) waitUntilStarted(ctx context.Context, candidate Backend) error {
	ctx, cancel := context.WithTimeout(ctx, f.config.StartupDeadline)
	defer cancel()
	client := &http.Client{
		Timeout: f.config.ProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	probeURL := "http://" + candidate.Address + candidate.StartupProbePath
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
			return ctx.Err()
		case <-timer.C:
		}
	}
}

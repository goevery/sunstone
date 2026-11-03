// Package routing runs Sunbeam's public proxy and private control
// interface: readiness-gated route switching, draining, durable route
// state, the HTTP proxy itself, and the loopback control transport.
package routing

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goevery/sunstone/internal/workload"
)

const (
	// DefaultControlAddress is Sunbeam's control endpoint on the VM host.
	DefaultControlAddress = "127.0.0.1:2025"
	// DefaultStatePath is the route state location intended for a Docker volume.
	DefaultStatePath = "/var/lib/sunbeam/routes.json"
	// defaultPageSize is ListRoutes' page size when page_size is unset.
	defaultPageSize = 50
	// maximumPageSize caps ListRoutes' page size.
	maximumPageSize = 100
)

// Route control sentinel errors.
var (
	// ErrNotFound indicates that a requested route does not exist.
	ErrNotFound = errors.New("route not found")
	// ErrFailedPrecondition indicates that another route already owns one
	// of the requested hostnames.
	ErrFailedPrecondition = errors.New("hostname is already routed by another route")
	// ErrInvalidArgument indicates invalid route-control input.
	ErrInvalidArgument = errors.New("invalid route request")
	// ErrBackendNotReady indicates that a candidate backend failed its
	// startup probe before any route change was applied, so the previous
	// route keeps serving.
	ErrBackendNotReady = errors.New("backend failed startup probe")
)

// Route describes one workload's active hostname routes and backend.
type Route struct {
	Name      string   `json:"name"`
	Hostnames []string `json:"hostnames"`
	Backend   Backend  `json:"backend"`
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

// routeState carries one route's mutation serialization and serving
// backend. The route's mutex is held across that route's whole mutation —
// startup probe, switch, and drain — so the router lock stays free for
// every other workload. backend is written only while committing a switch
// and read through the atomic pointer without further locking.
type routeState struct {
	mu      sync.Mutex
	backend atomic.Pointer[backend]
}

// Router owns public proxying, route state, startup checks, and draining.
// mu guards the route table and hostname bindings and is held only for
// brief claim-check, persist, and switch sections — never across a probe or
// a drain — so different workloads' mutations and traffic stay independent.
type Router struct {
	config RouterConfig

	mu       sync.RWMutex
	routes   map[string]Route       // route name → active route
	entries  map[string]*routeState // route name → state; an entry may outlive its route
	bindings map[string]string      // hostname → owning route name
}

// NewRouter restores durable state and constructs the router. Malformed
// persisted routes, or persisted routes that disagree about hostname
// ownership, fail construction; restored routes become active immediately.
func NewRouter(config RouterConfig) (*Router, error) {
	if config.StatePath == "" {
		return nil, errors.New("state path is required")
	}
	if config.ProbeTimeout <= 0 || config.ProbeInterval <= 0 || config.StartupDeadline <= 0 || config.DrainTimeout <= 0 {
		return nil, errors.New("routing durations must be positive")
	}

	routes, err := loadState(config.StatePath)
	if err != nil {
		return nil, err
	}

	router := &Router{
		config:   config,
		routes:   make(map[string]Route, len(routes)),
		entries:  make(map[string]*routeState, len(routes)),
		bindings: make(map[string]string),
	}
	for _, route := range routes {
		if err := validateRoute(route); err != nil {
			router.Close()
			return nil, fmt.Errorf("validate persisted route: %w", err)
		}
		if _, exists := router.routes[route.Name]; exists {
			router.Close()
			return nil, fmt.Errorf("persisted state repeats route %s", route.Name)
		}
		for _, hostname := range route.Hostnames {
			if owner, taken := router.bindings[hostname]; taken {
				router.Close()
				return nil, fmt.Errorf("persisted routes %s and %s both claim hostname %s", owner, route.Name, hostname)
			}
			router.bindings[hostname] = route.Name
		}
		entry := &routeState{}
		entry.backend.Store(newBackend(route))
		router.routes[route.Name] = route
		router.entries[route.Name] = entry
	}

	return router, nil
}

// Proxy forwards one public HTTP request to the backend bound to the
// request's hostname. A malformed authority is rejected; an unrouted
// hostname is not found; a backend mid-switch briefly refuses admission
// so the request can be retried against the successor.
func (r *Router) Proxy(writer http.ResponseWriter, request *http.Request) {
	hostname, ok := requestHostname(request.Host)
	if !ok {
		http.Error(writer, "invalid host", http.StatusBadRequest)
		return
	}
	for {
		r.mu.RLock()
		var current *backend
		if name, routed := r.bindings[hostname]; routed {
			current = r.entries[name].backend.Load()
		}
		r.mu.RUnlock()
		if current == nil {
			http.Error(writer, "not found", http.StatusNotFound)
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

	r.mu.RLock()
	defer r.mu.RUnlock()
	route, exists := r.routes[name]
	if !exists {
		return Route{}, ErrNotFound
	}
	route.Hostnames = slices.Clone(route.Hostnames)

	return route, nil
}

// ListRoutes returns one page of routes ordered by name, with an opaque
// token for the next page when more remain. The token encodes the last
// route of the page.
func (r *Router) ListRoutes(pageSize int32, pageToken string) ([]Route, string, error) {
	if pageSize < 0 {
		return nil, "", fmt.Errorf("%w: page_size must not be negative", ErrInvalidArgument)
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maximumPageSize {
		pageSize = maximumPageSize
	}
	cursor := ""
	if pageToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(pageToken)
		if err != nil {
			return nil, "", fmt.Errorf("%w: page_token is invalid", ErrInvalidArgument)
		}
		cursor = string(decoded)
		if err := validateRouteName(cursor); err != nil {
			return nil, "", fmt.Errorf("%w: page_token is invalid", ErrInvalidArgument)
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.routes))
	for name := range r.routes {
		names = append(names, name)
	}
	slices.Sort(names)

	routes := make([]Route, 0, min(int(pageSize), len(names)))
	for _, name := range names {
		if cursor != "" && name <= cursor {
			continue
		}
		if int32(len(routes)) == pageSize {
			return routes, encodePageToken(routes[len(routes)-1].Name), nil
		}
		route := r.routes[name]
		route.Hostnames = slices.Clone(route.Hostnames)
		routes = append(routes, route)
	}

	return routes, "", nil
}

// UpdateRoute validates, starts, persists, switches, and drains a route.
// The route's own mutex serializes this whole mutation, while the router
// lock is taken only for brief claim checks and the switch itself, so
// other workloads keep serving and mutating throughout the probe and the
// drain.
func (r *Router) UpdateRoute(ctx context.Context, requested Route, paths []string, allowMissing bool) (Route, error) {
	if err := validateRouteName(requested.Name); err != nil {
		return Route{}, err
	}
	if err := validateMask(paths); err != nil {
		return Route{}, err
	}

	entry, err := r.entry(requested.Name, allowMissing)
	if err != nil {
		return Route{}, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()

	r.mu.RLock()
	current, exists := r.routes[requested.Name]
	r.mu.RUnlock()
	if !exists && !allowMissing {
		return Route{}, ErrNotFound
	}
	var previous *Route
	if exists {
		stored := current
		previous = &stored
	}
	candidate := applyUpdateMask(previous, requested, paths)
	// The router owns what it stores and returns: no hostname slice shared
	// with a caller can reach route state.
	candidate.Hostnames = slices.Clone(candidate.Hostnames)
	if err := validateRoute(candidate); err != nil {
		return Route{}, err
	}
	if exists && routeEqual(current, candidate) {
		current.Hostnames = slices.Clone(current.Hostnames)

		return current, nil
	}
	if err := r.checkHostnameClaims(requested.Name, candidate.Hostnames); err != nil {
		return Route{}, err
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

	previousBackend := entry.backend.Load()
	if err := r.commitRoute(requested.Name, previous, previousBackend, candidate); err != nil {
		return Route{}, err
	}

	if previousBackend != nil {
		previousBackend.drain(r.config.DrainTimeout)
	}

	// The result's hostnames belong to the caller; the committed route
	// keeps its own copy.
	candidate.Hostnames = slices.Clone(candidate.Hostnames)

	return candidate, nil
}

// DeleteRoute durably removes a route, stops new traffic, and drains
// admitted requests. Other routes are untouched.
func (r *Router) DeleteRoute(name string, allowMissing bool) error {
	if err := validateRouteName(name); err != nil {
		return err
	}

	entry, err := r.entry(name, true)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()

	r.mu.RLock()
	route, exists := r.routes[name]
	r.mu.RUnlock()
	if !exists {
		if allowMissing {
			return nil
		}

		return ErrNotFound
	}

	previousBackend := entry.backend.Load()
	r.mu.Lock()
	remaining := make([]Route, 0, len(r.routes))
	for routeName, persisted := range r.routes {
		if routeName != name {
			remaining = append(remaining, persisted)
		}
	}
	if err := persistState(r.config.StatePath, remaining); err != nil {
		r.mu.Unlock()

		return fmt.Errorf("persist route deletion: %w", err)
	}
	// Admission closes before the removal is published, atomically with
	// it: a request holding the old backend pointer either acquired before
	// the deletion or is rejected and answered as unrouted.
	if previousBackend != nil {
		previousBackend.retire()
	}
	for _, hostname := range route.Hostnames {
		delete(r.bindings, hostname)
	}
	delete(r.routes, name)
	entry.backend.Store(nil)
	r.mu.Unlock()

	if previousBackend != nil {
		previousBackend.drain(r.config.DrainTimeout)
	}

	return nil
}

// Close cancels active backend work and releases idle connections.
func (r *Router) Close() {
	r.mu.RLock()
	backends := make([]*backend, 0, len(r.entries))
	for _, entry := range r.entries {
		if current := entry.backend.Load(); current != nil {
			backends = append(backends, current)
		}
	}
	r.mu.RUnlock()
	for _, current := range backends {
		current.stop()
	}
}

// entry returns the route's mutation-serialization state, creating an
// inactive placeholder when create is set. Placeholders serialize
// concurrent creations and later mutations of the same workload; they
// carry no route and stay invisible until a committed update installs one.
func (r *Router) entry(name string, create bool) (*routeState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, exists := r.entries[name]; exists {
		return entry, nil
	}
	if !create {
		return nil, ErrNotFound
	}
	entry := &routeState{}
	r.entries[name] = entry

	return entry, nil
}

// checkHostnameClaims rejects hostnames another route already owns,
// before a candidate spends its startup budget on a doomed switch.
func (r *Router) checkHostnameClaims(name string, hostnames []string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, hostname := range hostnames {
		if owner, taken := r.bindings[hostname]; taken && owner != name {
			return fmt.Errorf("%w: %s", ErrFailedPrecondition, hostname)
		}
	}

	return nil
}

// commitRoute durably and atomically switches one route to candidate:
// hostname claims are re-verified (another route may have claimed during
// the candidate's startup probe), the full route table is persisted, the
// previous backend stops admitting, and the route's hostnames, record,
// and serving backend switch together. Any failure leaves the previous
// routing untouched, including admission to the previous backend.
func (r *Router) commitRoute(name string, current *Route, previousBackend *backend, candidate Route) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, hostname := range candidate.Hostnames {
		if owner, taken := r.bindings[hostname]; taken && owner != name {
			return fmt.Errorf("%w: %s", ErrFailedPrecondition, hostname)
		}
	}
	snapshot := make([]Route, 0, len(r.routes)+1)
	for routeName, route := range r.routes {
		if routeName != name {
			snapshot = append(snapshot, route)
		}
	}
	snapshot = append(snapshot, candidate)
	if err := persistState(r.config.StatePath, snapshot); err != nil {
		return fmt.Errorf("persist route: %w", err)
	}

	// Admission closes before publication, atomically with the switch: a
	// request holding the old backend pointer either acquired before the
	// switch or is rejected and retried against the successor.
	if previousBackend != nil {
		previousBackend.retire()
	}
	if current != nil {
		for _, hostname := range current.Hostnames {
			if !slices.Contains(candidate.Hostnames, hostname) {
				delete(r.bindings, hostname)
			}
		}
	}
	for _, hostname := range candidate.Hostnames {
		r.bindings[hostname] = name
	}
	r.routes[name] = candidate
	r.entries[name].backend.Store(newBackend(candidate))

	return nil
}

// encodePageToken obfuscates a pagination cursor per AIP-158.
func encodePageToken(cursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

// requestHostname canonicalizes an HTTP request authority for hostname
// lookup. DNS names are case-insensitive and may carry an optional root
// dot, and an explicit port is not part of the name, so those are
// normalized away; configured hostnames themselves stay strict. The
// second result is false when the authority is malformed: brackets may
// only enclose a valid IPv6 literal — which no configured hostname can
// ever match — and a DNS authority must satisfy the canonical hostname
// rules.
func requestHostname(authority string) (string, bool) {
	host := authority
	if strings.HasPrefix(host, "[") {
		// A bracketed authority is well formed only as an IPv6 literal,
		// so its canonical form is looked up to miss as unrouted.
		end := strings.Index(host, "]")
		if end < 0 {
			return "", false
		}
		if rest := host[end+1:]; rest != "" && (rest[0] != ':' || !validPort(rest[1:])) {
			return "", false
		}
		literal := host[1:end]
		if !strings.Contains(literal, ":") || net.ParseIP(literal) == nil {
			return "", false
		}

		return strings.ToLower(literal), true
	}
	if index := strings.LastIndex(host, ":"); index >= 0 {
		if !validPort(host[index+1:]) {
			return "", false
		}
		host = host[:index]
	}
	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	if err := workload.ValidateHostname(host); err != nil {
		return "", false
	}

	return host, true
}

// validPort reports whether port is a decimal port number.
func validPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return false
		}
	}

	return true
}

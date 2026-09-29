// Package routing runs Sunbeam's public proxy and loopback control interface.
package routing

import (
	"context"
	"errors"
	"net"
	"time"

	routingfeature "github.com/goevery/sunstone/internal/modules/routing/internal/features/routing"
)

const (
	// DefaultControlAddress is Sunbeam's loopback control endpoint.
	DefaultControlAddress = "127.0.0.1:2025"
	// DefaultStatePath is the route state location intended for a Docker volume.
	DefaultStatePath = "/var/lib/sunbeam/routes.json"
)

// Config contains the listeners, durable state, and bounded operation policies.
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

// Module serves HTTP traffic and route-control requests until canceled.
type Module interface {
	Serve(context.Context) error
}

// New constructs a routing module and restores its durable route.
func New(config Config) (Module, error) {
	if config.TrafficListener == nil || config.ControlListener == nil {
		return nil, errors.New("traffic and control listeners are required")
	}
	controlAddress, ok := config.ControlListener.Addr().(*net.TCPAddr)
	if !ok || controlAddress.IP == nil || !controlAddress.IP.IsLoopback() {
		return nil, errors.New("control listener must use a loopback TCP address")
	}
	if config.StatePath == "" {
		return nil, errors.New("state path is required")
	}
	if config.ProbeTimeout <= 0 || config.ProbeInterval <= 0 || config.StartupDeadline <= 0 || config.DrainTimeout <= 0 || config.ShutdownTimeout <= 0 {
		return nil, errors.New("routing durations must be positive")
	}

	return routingfeature.New(routingfeature.Config(config))
}

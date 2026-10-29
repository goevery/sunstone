package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goevery/sunstone/internal/routing"
)

const (
	trafficAddress  = ":8080"
	controlAddress  = ":2025"
	probeTimeout    = time.Second
	probeInterval   = time.Second
	startupDeadline = 5 * time.Minute
	drainTimeout    = 30 * time.Second
	shutdownTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run owns both listeners: a constructor failure at any later step closes
// them, and the server borrows them for the life of the command.
func run() error {
	traffic, err := net.Listen("tcp", trafficAddress)
	if err != nil {
		return fmt.Errorf("listen for HTTP traffic: %w", err)
	}
	control, err := net.Listen("tcp", controlAddress)
	if err != nil {
		traffic.Close()

		return fmt.Errorf("listen for control requests: %w", err)
	}

	router, err := routing.NewRouter(routing.RouterConfig{
		StatePath:       routing.DefaultStatePath,
		ProbeTimeout:    probeTimeout,
		ProbeInterval:   probeInterval,
		StartupDeadline: startupDeadline,
		DrainTimeout:    drainTimeout,
	})
	if err != nil {
		return errors.Join(err, traffic.Close(), control.Close())
	}
	server, err := routing.NewServer(routing.ServerConfig{
		TrafficListener: traffic,
		ControlListener: control,
		Router:          router,
		ShutdownTimeout: shutdownTimeout,
	})
	if err != nil {
		router.Close()

		return errors.Join(err, traffic.Close(), control.Close())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return server.Serve(ctx)
}

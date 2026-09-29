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

	"github.com/goevery/sunstone/internal/modules/routing"
)

const (
	trafficAddress  = ":8080"
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

func run() error {
	traffic, err := net.Listen("tcp", trafficAddress)
	if err != nil {
		return fmt.Errorf("listen for HTTP traffic: %w", err)
	}
	control, err := net.Listen("tcp", routing.DefaultControlAddress)
	if err != nil {
		traffic.Close()
		return fmt.Errorf("listen for control requests: %w", err)
	}
	module, err := routing.New(routing.Config{
		TrafficListener: traffic,
		ControlListener: control,
		StatePath:       routing.DefaultStatePath,
		ProbeTimeout:    probeTimeout,
		ProbeInterval:   probeInterval,
		StartupDeadline: startupDeadline,
		DrainTimeout:    drainTimeout,
		ShutdownTimeout: shutdownTimeout,
	})
	if err != nil {
		return errors.Join(err, traffic.Close(), control.Close())
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return module.Serve(ctx)
}

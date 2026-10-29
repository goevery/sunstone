package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/host"
)

func TestNewGCPHostProviderRequiresSessionOpener(t *testing.T) {
	if _, err := newGCPHostProvider(sunbeamControlAddress, nil); err == nil {
		t.Fatal("accepted a missing session opener")
	}
}

func TestNewGCPHostProviderRequiresControlAddress(t *testing.T) {
	uncalled := func(context.Context, deployment.Target) (targetSession, error) {
		return nil, errors.New("unreachable")
	}
	if _, err := newGCPHostProvider("", uncalled); err == nil {
		t.Fatal("accepted a missing control address")
	}
}

func TestOpenReusesOneTransportAcrossOperations(t *testing.T) {
	var opens, closes int
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(context.Context, deployment.Target) (targetSession, error) {
		opens++

		return recordingSession{closed: &closes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := deployment.Target{Project: "acme-prod", Zone: "us-central1-a", Instance: "web-1"}
	report := func(host.Progress) {}

	first, err := provider.Open(t.Context(), target, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A second operation on the same target borrows the same transport but
	// gets its own host and runtime.
	second, err := provider.Open(t.Context(), target, report)
	if err != nil {
		t.Fatal(err)
	}
	if first.Host == second.Host {
		t.Fatal("operations shared one host")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("session opens = %d", opens)
	}
	// Handle close releases operation resources only; the command-owned
	// session is untouched until the command exits.
	if closes != 0 {
		t.Fatalf("session closes = %d", closes)
	}

	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if closes != 1 {
		t.Fatalf("session closes = %d", closes)
	}
}

func TestOpenUsesSeparateSessionsPerTarget(t *testing.T) {
	closes := map[string]*int{"web-1": new(int), "web-2": new(int)}
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(_ context.Context, target deployment.Target) (targetSession, error) {
		return recordingSession{closed: closes[target.Instance]}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	report := func(host.Progress) {}
	for _, instance := range []string{"web-1", "web-2"} {
		if _, err := provider.Open(t.Context(), deployment.Target{Project: "p", Zone: "z", Instance: instance}, report); err != nil {
			t.Fatal(err)
		}
	}

	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	for instance, count := range closes {
		if *count != 1 {
			t.Fatalf("session closes for %s = %d", instance, *count)
		}
	}
}

func TestProviderCloseIsIdempotentAndOpenFailsAfterClose(t *testing.T) {
	var opens, closes int
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(context.Context, deployment.Target) (targetSession, error) {
		opens++

		return recordingSession{closed: &closes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := deployment.Target{Project: "p", Zone: "z", Instance: "web-1"}
	report := func(host.Progress) {}
	if _, err := provider.Open(t.Context(), target, report); err != nil {
		t.Fatal(err)
	}

	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if closes != 1 {
		t.Fatalf("session closes = %d", closes)
	}
	if _, err := provider.Open(t.Context(), target, report); err == nil || !strings.Contains(err.Error(), "provider is closed") {
		t.Fatalf("error = %v", err)
	}
	if opens != 1 {
		t.Fatalf("open after close acquired a session: opens = %d", opens)
	}
}

func TestConcurrentOpenSharesOneSessionPerTarget(t *testing.T) {
	var opens, closes atomic.Int64
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(context.Context, deployment.Target) (targetSession, error) {
		opens.Add(1)

		return countingSession{closed: &closes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := deployment.Target{Project: "p", Zone: "z", Instance: "web-1"}
	report := func(host.Progress) {}

	var group sync.WaitGroup
	for attempt := 0; attempt < 4; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := provider.Open(context.Background(), target, report); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()

	if opens.Load() != 1 {
		t.Fatalf("concurrent opens established %d sessions", opens.Load())
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 1 {
		t.Fatalf("session closes = %d", closes.Load())
	}
}

func TestOpenRejectsNilReporterBeforeConnecting(t *testing.T) {
	var opens, closes int
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(context.Context, deployment.Target) (targetSession, error) {
		opens++

		return recordingSession{closed: &closes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A missing reporter is rejected construction, not an auth or network
	// failure: no session is opened or closed for it.
	if _, err := provider.Open(t.Context(), deployment.Target{}, nil); err == nil || !strings.Contains(err.Error(), "progress reporter is required") {
		t.Fatalf("error = %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if opens != 0 || closes != 0 {
		t.Fatalf("opens = %d, closes = %d", opens, closes)
	}
}

func TestOpenReportsSessionFailures(t *testing.T) {
	var opens, closes int
	provider, err := newGCPHostProvider(sunbeamControlAddress, func(context.Context, deployment.Target) (targetSession, error) {
		opens++

		return nil, errors.New("IAP unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Open(t.Context(), deployment.Target{}, func(host.Progress) {}); err == nil || err.Error() != "IAP unavailable" {
		t.Fatalf("error = %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if opens != 1 || closes != 0 {
		t.Fatalf("opens = %d, closes = %d", opens, closes)
	}
}

// recordingSession is a targetSession test double counting Close calls.
type recordingSession struct {
	closed *int
}

func (recordingSession) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("session is a test double")
}

func (s recordingSession) Close() error {
	*s.closed++

	return nil
}

// countingSession is a concurrent-safe targetSession test double.
type countingSession struct {
	closed *atomic.Int64
}

func (countingSession) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("session is a test double")
}

func (s countingSession) Close() error {
	s.closed.Add(1)

	return nil
}

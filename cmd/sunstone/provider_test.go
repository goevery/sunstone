package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

// TestProductionProviderLoadsSameKeyAcrossCalls verifies that repeated
// provider construction reuses the persisted key instead of generating a
// new one. Construction alone performs no network access.
func TestProductionProviderLoadsSameKeyAcrossCalls(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	account := "deployer@sunstone-test.iam.gserviceaccount.com"

	if _, err := productionProvider(account); err != nil {
		t.Fatalf("first productionProvider: %v", err)
	}
	first, err := os.ReadFile(productionKeyPath(dataHome, account))
	if err != nil {
		t.Fatalf("read key after first construction: %v", err)
	}

	if _, err := productionProvider(account); err != nil {
		t.Fatalf("second productionProvider: %v", err)
	}
	second, err := os.ReadFile(productionKeyPath(dataHome, account))
	if err != nil {
		t.Fatalf("read key after second construction: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second construction replaced the persisted key")
	}
}

func TestProductionProviderSeparatesIdentities(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	accounts := []string{
		"deployer@sunstone-test.iam.gserviceaccount.com",
		"status-bot@sunstone-test.iam.gserviceaccount.com",
	}

	for _, account := range accounts {
		if _, err := productionProvider(account); err != nil {
			t.Fatalf("productionProvider(%s): %v", account, err)
		}
	}

	keys := make([][]byte, len(accounts))
	for i, account := range accounts {
		key, err := os.ReadFile(productionKeyPath(dataHome, account))
		if err != nil {
			t.Fatalf("read key for %s: %v", account, err)
		}
		keys[i] = key
	}
	if bytes.Equal(keys[0], keys[1]) {
		t.Error("identities shared one key file")
	}
}

func TestProductionProviderHonorsXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	account := "deployer@sunstone-test.iam.gserviceaccount.com"

	if _, err := productionProvider(account); err != nil {
		t.Fatalf("productionProvider: %v", err)
	}
	if _, err := os.Stat(productionKeyPath(dataHome, account)); err != nil {
		t.Fatalf("key was not created under XDG_DATA_HOME: %v", err)
	}
}

func TestProductionProviderDefaultsToHomeLocalShare(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	account := "deployer@sunstone-test.iam.gserviceaccount.com"

	if _, err := productionProvider(account); err != nil {
		t.Fatalf("productionProvider: %v", err)
	}
	if _, err := os.Stat(productionKeyPath(filepath.Join(home, ".local", "share"), account)); err != nil {
		t.Fatalf("key was not created under ~/.local/share: %v", err)
	}
}

// TestProductionProviderCreatesMissingDataHome verifies the XDG root is
// created on first use when it does not exist yet.
func TestProductionProviderCreatesMissingDataHome(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "xdg", "data")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	account := "deployer@sunstone-test.iam.gserviceaccount.com"

	if _, err := productionProvider(account); err != nil {
		t.Fatalf("productionProvider: %v", err)
	}
	if _, err := os.Stat(productionKeyPath(dataHome, account)); err != nil {
		t.Fatalf("key was not created under the missing XDG data home: %v", err)
	}
}

func TestProductionProviderRejectsRelativeXDGDataHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "relative/data")
	t.Chdir(t.TempDir())

	if _, err := productionProvider("deployer@sunstone-test.iam.gserviceaccount.com"); err == nil {
		t.Fatal("relative XDG_DATA_HOME was accepted")
	}
	if _, err := os.Stat("relative"); err == nil {
		t.Fatal("relative XDG_DATA_HOME created directories in the working directory")
	}
}

func TestProductionProviderRejectsInvalidIdentity(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "xdg", "data")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)

	if _, err := productionProvider("not-an-email"); err == nil {
		t.Fatal("invalid service account was accepted")
	}
	if _, err := os.Stat(dataHome); err == nil {
		t.Fatal("invalid service account created the data home")
	}
}

// productionKeyPath returns the persisted key location for one service
// account under dataHome.
func productionKeyPath(dataHome, serviceAccount string) string {
	return filepath.Join(dataHome, "sunstone", "ssh", serviceAccount, "id_ed25519")
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

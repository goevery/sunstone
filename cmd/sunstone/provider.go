package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/session"
)

// The command-owned provider satisfies the group's provider seam and the
// command's closable provider.
var _ commandProvider = (*gcpHostProvider)(nil)

// sunbeamControlAddress is the loopback-only Sunbeam control endpoint on
// each target VM, provided explicitly to the runtime constructor.
const sunbeamControlAddress = "127.0.0.1:2025"

// cleanupTimeout bounds the host's interruption cleanup, matching the
// runtime's own per-request bound.
const cleanupTimeout = 30 * time.Second

// targetSession is one established, reusable transport to a target VM. The
// provider owns it for the life of the command; the runtimes it is lent to
// never close it.
type targetSession interface {
	host.Session
	Close() error
}

// gcpHostProvider opens per-target hosts for the group while caching one
// command-owned session per target: the first Open establishes the
// transport, later Opens reuse it, and Close tears every session down at
// command exit. Each Open builds a fresh operation-scoped runtime and host
// over the borrowed session, so secret snapshots and redaction values never
// outlive their operation. Established sessions are never re-established or
// evicted; a broken one stays broken.
type gcpHostProvider struct {
	controlAddress string
	openSession    func(context.Context, deployment.Target) (targetSession, error)

	mu       sync.Mutex
	closed   bool
	sessions map[deployment.Target]targetSession
}

// newGCPHostProvider constructs the provider from its mandatory
// collaborators; the session cache starts empty and no resources are
// acquired until the first Open.
func newGCPHostProvider(controlAddress string, openSession func(context.Context, deployment.Target) (targetSession, error)) (*gcpHostProvider, error) {
	if openSession == nil {
		return nil, errors.New("session opener is required")
	}
	if controlAddress == "" {
		return nil, errors.New("Sunbeam control address is required")
	}

	return &gcpHostProvider{
		controlAddress: controlAddress,
		openSession:    openSession,
		sessions:       make(map[deployment.Target]targetSession),
	}, nil
}

// productionProvider composes the Google Cloud session stack for one command
// run: the persistent SSH signer, the known-hosts source, and the GCP
// connector impersonating serviceAccount, feeding the command-owned session
// cache. The signer's key is stored under the XDG data home, defaulting to
// ~/.local/share, and keyed by the impersonated service account.
func productionProvider(serviceAccount string) (commandProvider, error) {
	if serviceAccount == "" {
		return nil, errors.New("service account to impersonate is required")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("find home directory for SSH known hosts: %v", err)
	}

	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}

	signer, err := session.LoadSigner(dataHome, serviceAccount)
	if err != nil {
		return nil, err
	}
	sessions, err := session.NewGCPConnector(filepath.Join(home, ".ssh", "google_compute_known_hosts"), signer)
	if err != nil {
		return nil, err
	}
	openSession := func(ctx context.Context, target deployment.Target) (targetSession, error) {
		return sessions.Connect(
			ctx,
			session.Target{Project: target.Project, Zone: target.Zone, Instance: target.Instance},
			session.Identity{ServiceAccount: serviceAccount},
		)
	}

	return newGCPHostProvider(sunbeamControlAddress, openSession)
}

// Open supplies the target with a fresh operation-scoped host over its
// cached session, establishing the session on first use. The reporter is
// validated before any connection resource is acquired, so rejected
// construction never reaches the network. A session stays cached even when
// building the runtime or host fails; the provider closes it at command
// exit, and the runtime built over it is released here.
func (p *gcpHostProvider) Open(ctx context.Context, target deployment.Target, report host.Reporter) (deployment.Handle, error) {
	if report == nil {
		return deployment.Handle{}, errors.New("progress reporter is required")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return deployment.Handle{}, errors.New("provider is closed")
	}
	sess, cached := p.sessions[target]
	if !cached {
		opened, err := p.openSession(ctx, target)
		if err != nil {
			return deployment.Handle{}, err
		}
		p.sessions[target] = opened
		sess = opened
	}

	runtime, err := host.NewDockerRuntime(sess, p.controlAddress)
	if err != nil {
		return deployment.Handle{}, err
	}

	vm, err := host.NewHost(runtime, report, host.HostConfig{CleanupTimeout: cleanupTimeout})
	if err != nil {
		closeErr := runtime.Close()

		return deployment.Handle{}, errors.Join(err, closeErr)
	}

	return deployment.Handle{Host: vm, Close: runtime.Close}, nil
}

// Close tears down every cached session exactly once. It is idempotent, and
// Open fails after it without acquiring new sessions.
func (p *gcpHostProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true

	var closeErr error
	for target, sess := range p.sessions {
		closeErr = errors.Join(closeErr, sess.Close())
		delete(p.sessions, target)
	}

	return closeErr
}

package session

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// establishSession opens a tunnel through dialTunnel and performs the SSH
// handshake over it, owning the tunnel's lifecycle explicitly. A tunnel's
// data path is bound to the context it was dialed with, so the tunnel is
// dialed under a lifecycle context that follows the setup context's
// cancellation until the handshake succeeds; on success the setup
// cancellation is detached and the established session dies only when its
// owner closes it. If the setup context is canceled while the handshake
// completes, the session is closed and the cancellation is returned instead
// of a session whose transport is already dying. A failed establishment
// always releases the tunnel and its lifecycle context.
func establishSession(setup context.Context, dialTunnel func(context.Context) (net.Conn, error), handshake Handshake) (*Session, error) {
	lifeCtx, lifeCancel := context.WithCancel(context.WithoutCancel(setup))

	// The follower translates setup cancellation into lifecycle
	// cancellation until it is detached, so a stalled dial or handshake
	// aborts when the setup context ends.
	detach := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-setup.Done():
			lifeCancel()
		case <-detach:
		}
	}()

	tunnel, err := dialTunnel(lifeCtx)
	if err != nil {
		detachFollow(detach, stopped)
		lifeCancel()
		if setupErr := setup.Err(); setupErr != nil {
			// The setup context ended first: preserve its identity — the
			// derived cancellation the tunnel saw is only a consequence.
			return nil, errors.Join(err, setupErr)
		}

		return nil, err
	}
	conn := &lifecycleConn{Conn: tunnel, cancel: lifeCancel}

	session, sessionErr := NewSession(conn, handshake)
	if sessionErr != nil {
		// NewSession owns the connection on failure and closed it, which
		// released the lifecycle context.
		detachFollow(detach, stopped)
		if setupErr := setup.Err(); setupErr != nil {
			// The setup context ended first: preserve its identity — the
			// tunnel died with the cancellation it caused.
			return nil, errors.Join(sessionErr, setupErr)
		}

		return nil, sessionErr
	}

	// Detach the setup cancellation, then verify the setup context still
	// holds: a cancellation that raced with the handshake must not leave a
	// dying session in the caller's hands.
	detachFollow(detach, stopped)
	if err := setup.Err(); err != nil {
		session.Close()

		return nil, fmt.Errorf("session established while setup canceled: %w", err)
	}

	return session, nil
}

// detachFollow stops the setup-cancellation follower and waits for it, so no
// setup cancellation can reach the lifecycle context afterwards.
func detachFollow(detach, stopped chan struct{}) {
	close(detach)
	<-stopped
}

// lifecycleConn releases the tunnel's lifecycle context when the connection
// closes, so closing the session tears down the tunnel and its context
// together.
type lifecycleConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *lifecycleConn) Close() error {
	c.cancel()

	return c.Conn.Close()
}

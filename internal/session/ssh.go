// Package session establishes SSH sessions to remote hosts, including
// IAP-protected transports to Compute Engine VMs.
package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const sshHandshakeLimit = 30 * time.Second

var errSSHAuthentication = errors.New("SSH authentication rejected")

// Handshake is the required SSH handshake configuration. It carries the
// caller's authentication and host-key policy; there is no default or
// insecure fallback.
type Handshake struct {
	User            string
	Signer          ssh.Signer
	HostAlias       string
	HostKeyCallback ssh.HostKeyCallback
}

// Session is one established SSH connection to a remote host.
type Session struct {
	ssh *ssh.Client
}

// DialContext opens one channel over the established SSH connection for a
// remote Unix or TCP socket. It never reconnects; if the connection is gone,
// DialContext fails and the caller decides what to do.
func (s *Session) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return s.ssh.DialContext(ctx, network, address)
}

// Close tears down the SSH connection and its underlying transport.
func (s *Session) Close() error {
	return s.ssh.Close()
}

// NewSession performs the SSH handshake over conn, which must already be
// connected to the remote SSH server. It takes ownership of conn: once a
// non-nil conn is supplied, NewSession owns it and closes it on any failure,
// including invalid handshake configuration, so callers never need to
// distinguish error kinds to avoid leaks. Close closes conn as part of the
// session.
func NewSession(conn net.Conn, handshake Handshake) (*Session, error) {
	if conn == nil {
		return nil, errors.New("connection is required")
	}
	if err := handshake.validate(); err != nil {
		conn.Close()

		return nil, err
	}

	if err := conn.SetDeadline(time.Now().Add(sshHandshakeLimit)); err != nil {
		conn.Close()

		return nil, fmt.Errorf("set SSH handshake deadline: %w", err)
	}

	connection, channels, requests, err := ssh.NewClientConn(conn, handshake.HostAlias, &ssh.ClientConfig{
		User:              handshake.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(handshake.Signer)},
		HostKeyCallback:   handshake.HostKeyCallback,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	})
	if err != nil {
		conn.Close()

		if strings.Contains(err.Error(), "ssh: unable to authenticate") {
			return nil, fmt.Errorf("%w: %v", errSSHAuthentication, err)
		}

		return nil, fmt.Errorf("SSH handshake: %w", err)
	}

	sshClient := ssh.NewClient(connection, channels, requests)
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sshClient.Close()

		return nil, fmt.Errorf("clear SSH handshake deadline: %w", err)
	}

	return &Session{ssh: sshClient}, nil
}

func (handshake Handshake) validate() error {
	switch {
	case handshake.User == "":
		return errors.New("SSH user is required")
	case handshake.Signer == nil:
		return errors.New("SSH signer is required")
	case handshake.HostAlias == "":
		return errors.New("SSH host alias is required")
	case handshake.HostKeyCallback == nil:
		return errors.New("SSH host key callback is required")
	}

	return nil
}

// GenerateSigner creates an in-memory ED25519 signer without persisting it.
func GenerateSigner() (ssh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral SSH key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("create SSH signer: %w", err)
	}

	return signer, nil
}

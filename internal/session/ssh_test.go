package session

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestGeneratesUsableEphemeralKey(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	challenge := []byte("sunstone")
	signature, err := signer.Sign(rand.Reader, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.PublicKey().Verify(challenge, signature); err != nil {
		t.Fatalf("generated key cannot verify its signature: %v", err)
	}
}

func TestNewSessionRejectsMissingInputs(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	recorded := &recordingConn{Conn: conn}
	t.Cleanup(func() { conn.Close() })

	valid := Handshake{User: "operator", Signer: signer, HostAlias: "test-vm", HostKeyCallback: ssh.InsecureIgnoreHostKey()}
	invalid := map[string]Handshake{
		"missing user":            {User: "", Signer: signer, HostAlias: "test-vm", HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		"missing signer":          {User: "operator", Signer: nil, HostAlias: "test-vm", HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		"missing host alias":      {User: "operator", Signer: signer, HostAlias: "", HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		"missing host key policy": {User: "operator", Signer: signer, HostAlias: "test-vm", HostKeyCallback: nil},
	}
	for name, handshake := range invalid {
		before := recorded.closeCount()
		if _, err := NewSession(recorded, handshake); err == nil {
			t.Fatalf("%s: accepted invalid handshake configuration", name)
		}
		if recorded.closeCount() != before+1 {
			t.Fatalf("%s: owned connection not closed on failure (closes = %d)", name, recorded.closeCount())
		}
	}

	if _, err := NewSession(nil, valid); err == nil {
		t.Fatal("accepted a missing connection")
	}
}

func TestNewSessionClosesOwnedConnectionOnAnyFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	recorded := &recordingConn{Conn: conn}
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSession(recorded, Handshake{User: "operator", Signer: signer, HostAlias: "test-vm", HostKeyCallback: ssh.InsecureIgnoreHostKey()}); err == nil {
		t.Fatal("handshake succeeded against a closed server")
	}
	if recorded.closeCount() == 0 {
		t.Fatal("connection left open after handshake failure")
	}
}

func TestNewSessionClearsHandshakeDeadline(t *testing.T) {
	session, recorded := newTestSession(t)
	defer session.Close()

	deadlines := recorded.recordedDeadlines()
	if len(deadlines) < 2 {
		t.Fatalf("expected handshake deadline then clear, got %v", deadlines)
	}
	if deadlines[len(deadlines)-1] != (time.Time{}) {
		t.Fatalf("handshake deadline still applied: %v", deadlines[len(deadlines)-1])
	}
	for _, deadline := range deadlines[:len(deadlines)-1] {
		if deadline.IsZero() {
			t.Fatalf("handshake deadline was not bounded: %v", deadlines)
		}
	}
}

func TestDialContextCarriesUnixAndTCPChannels(t *testing.T) {
	session, _ := newTestSession(t)
	defer session.Close()
	for _, dial := range []struct {
		network string
		address string
	}{
		{network: "tcp", address: "127.0.0.1:8080"},
		{network: "unix", address: "/var/run/docker.sock"},
	} {
		conn, err := session.DialContext(t.Context(), dial.network, dial.address)
		if err != nil {
			t.Fatalf("dial %s %s: %v", dial.network, dial.address, err)
		}
		message := []byte("sunstone")
		if _, err := conn.Write(message); err != nil {
			t.Fatalf("write over %s channel: %v", dial.network, err)
		}
		echo := make([]byte, len(message))
		if _, err := io.ReadFull(conn, echo); err != nil {
			t.Fatalf("read over %s channel: %v", dial.network, err)
		}
		if !bytes.Equal(echo, message) {
			t.Fatalf("echo over %s channel = %q, want %q", dial.network, echo, message)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close %s channel: %v", dial.network, err)
		}
	}
}

func TestDialContextDoesNotReconnectAfterClose(t *testing.T) {
	session, _ := newTestSession(t)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := session.DialContext(t.Context(), "tcp", "127.0.0.1:8080"); err == nil {
		t.Fatal("dial after close succeeded; session reconnected silently")
	}
}

// newTestSession establishes a session over a local SSH server that echoes
// channel data, without reaching Google Cloud.
func newTestSession(t *testing.T) (*Session, *recordingConn) {
	t.Helper()

	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	listener, server := echoServer(t, signer.PublicKey())
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	recorded := &recordingConn{Conn: conn}
	go server()

	session, err := NewSession(recorded, Handshake{
		User:            "operator",
		Signer:          signer,
		HostAlias:       "test-vm",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("open test session: %v", err)
	}

	return session, recorded
}

// echoServer listens on loopback and returns the listener plus a function
// that accepts one connection and serves an SSH server on it that only admits
// the given client key and echoes channel data.
func echoServer(t *testing.T, clientKey ssh.PublicKey) (net.Listener, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	return listener, func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		hostKey, err := GenerateSigner()
		if err != nil {
			conn.Close()

			return
		}

		config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientKey.Marshal()) {
				return nil, errors.New("unknown client key")
			}
			return nil, nil
		}}
		config.AddHostKey(hostKey)
		serveSSH(conn, config)
	}
}

func serveSSH(conn net.Conn, config *ssh.ServerConfig) {
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer server.Close()

	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "direct-tcpip" && newChannel.ChannelType() != "direct-streamlocal@openssh.com" {
			newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}

		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(channelRequests)
		go func() {
			defer channel.Close()
			_, _ = io.Copy(channel, channel)
		}()
	}
}

// recordingConn records deadline and close calls on the underlying connection.
type recordingConn struct {
	net.Conn

	mu        sync.Mutex
	deadlines []time.Time
	closes    int
}

func (c *recordingConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()

	return c.Conn.SetDeadline(deadline)
}

func (c *recordingConn) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()

	return c.Conn.Close()
}

func (c *recordingConn) recordedDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]time.Time(nil), c.deadlines...)
}

func (c *recordingConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closes
}

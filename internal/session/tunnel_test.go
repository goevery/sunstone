package session

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestEstablishedSessionSurvivesSetupCancellation(t *testing.T) {
	signer, handshake := testHandshake(t)
	listener, server := echoServer(t, signer.PublicKey())
	go server()

	setup, cancelSetup := context.WithCancel(t.Context())
	session, err := establishSession(setup, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}, handshake)
	if err != nil {
		t.Fatal(err)
	}

	// The setup context ends with preflight; the established session owns
	// its transport lifetime and keeps working.
	cancelSetup()
	message := []byte("sunstone")
	conn, err := session.DialContext(t.Context(), "tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("dial after setup cancellation: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(message); err != nil {
		t.Fatalf("write after setup cancellation: %v", err)
	}
	echo := make([]byte, len(message))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read after setup cancellation: %v", err)
	}

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.DialContext(t.Context(), "tcp", "127.0.0.1:8080"); err == nil {
		t.Fatal("session reconnected after close")
	}
}

func TestSessionCloseReleasesTunnel(t *testing.T) {
	signer, handshake := testHandshake(t)
	listener, server := echoServer(t, signer.PublicKey())
	go server()

	var tunnel *recordingConn
	session, err := establishSession(t.Context(), func(ctx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		if err != nil {
			return nil, err
		}
		tunnel = &recordingConn{Conn: conn}

		return tunnel, nil
	}, handshake)
	if err != nil {
		t.Fatal(err)
	}

	// Closing the session tears the tunnel down together with its
	// lifecycle context, and the session stays down.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if closes := tunnel.closeCount(); closes < 1 {
		t.Fatalf("tunnel closes = %d", closes)
	}
	if _, err := session.DialContext(t.Context(), "tcp", "127.0.0.1:8080"); err == nil {
		t.Fatal("session reconnected after close")
	}
}

func TestSetupDeadlineIdentityPreservedWhenDialStalls(t *testing.T) {
	_, handshake := testHandshake(t)
	setup, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	dial := func(lifeCtx context.Context) (net.Conn, error) {
		// Stall until the lifecycle context ends.
		<-lifeCtx.Done()

		return nil, lifeCtx.Err()
	}

	_, err := establishSession(setup, dial, handshake)
	// The tunnel died with a derived cancellation, but the setup context's
	// own deadline must stay identifiable to callers.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline identity preserved", err)
	}
}

func TestSetupCancellationDuringStalledDialAbortsEstablishment(t *testing.T) {
	_, handshake := testHandshake(t)
	setup, cancelSetup := context.WithCancel(t.Context())
	ready := make(chan struct{})
	dialed := make(chan context.Context, 1)
	dial := func(lifeCtx context.Context) (net.Conn, error) {
		dialed <- lifeCtx
		close(ready)
		// Stall until the lifecycle context ends.
		<-lifeCtx.Done()

		return nil, lifeCtx.Err()
	}

	result := make(chan error, 1)
	go func() {
		_, err := establishSession(setup, dial, handshake)
		result <- err
	}()
	<-ready
	cancelSetup()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled setup did not abort the stalled dial")
	}
	// Setup cancellation reached the tunnel's lifecycle context.
	if lifeCtx := <-dialed; lifeCtx.Err() == nil {
		t.Fatal("tunnel lifecycle context survived setup cancellation")
	}
}

func TestSetupCancellationDuringStalledHandshakeClosesTunnel(t *testing.T) {
	_, handshake := testHandshake(t)
	// A server that accepts connections but never speaks SSH, leaving the
	// handshake stalled on its first read.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		accepted <- struct{}{}
		io.Copy(io.Discard, conn)
	}()

	setup, cancelSetup := context.WithCancel(t.Context())
	defer cancelSetup()
	var tunnel *recordingConn
	dial := func(lifeCtx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(lifeCtx, "tcp", listener.Addr().String())
		if err != nil {
			return nil, err
		}
		tunnel = &recordingConn{Conn: conn}
		// Mimic the IAP tunnel's context binding: the data path dies with
		// the context the tunnel was dialed under.
		go func() {
			<-lifeCtx.Done()
			conn.Close()
		}()

		return tunnel, nil
	}

	result := make(chan error, 1)
	go func() {
		_, err := establishSession(setup, dial, handshake)
		result <- err
	}()
	<-accepted
	cancelSetup()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled handshake succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled setup did not abort the stalled handshake")
	}
	// The stalled tunnel was released.
	if closes := tunnel.closeCount(); closes < 1 {
		t.Fatalf("tunnel closes = %d", closes)
	}
}

func TestEstablishSessionReturnsCancellationInsteadOfDyingSession(t *testing.T) {
	signer, handshake := testHandshake(t)
	listener, server := echoServer(t, signer.PublicKey())
	go server()

	setup, cancelSetup := context.WithCancel(t.Context())
	defer cancelSetup()
	var tunnel *recordingConn
	dial := func(context.Context) (net.Conn, error) {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			return nil, err
		}
		tunnel = &recordingConn{Conn: conn}
		// The tunnel finishes dialing while the operator cancels the
		// setup: the handshake still completes, but the established
		// session must be closed and the cancellation returned rather
		// than handed to the caller.
		cancelSetup()

		return tunnel, nil
	}

	session, err := establishSession(setup, dial, handshake)
	if session != nil {
		t.Fatalf("session = %+v, want nil", session)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	// The completed session was closed instead of handed over, releasing
	// its tunnel.
	if closes := tunnel.closeCount(); closes < 1 {
		t.Fatalf("tunnel closes = %d", closes)
	}
}

// testHandshake returns the handshake configuration and its signer for the
// local test SSH server.
func testHandshake(t *testing.T) (ssh.Signer, Handshake) {
	t.Helper()

	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}

	return signer, Handshake{
		User:            "operator",
		Signer:          signer,
		HostAlias:       "test-vm",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
}

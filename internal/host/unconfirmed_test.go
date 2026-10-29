package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/errdefs"
)

func TestUnconfirmedErrorPreservesCauseIdentity(t *testing.T) {
	cause := errors.New("read tcp: connection reset by peer")
	err := unconfirmed("start container", cause)

	var marker *UnconfirmedError
	if !errors.As(err, &marker) {
		t.Fatalf("marker not detectable: %v", err)
	}
	if marker.Operation != "start container" || !errors.Is(err, cause) {
		t.Fatalf("marker = %+v", marker)
	}
	if !strings.Contains(err.Error(), "start container") || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestUnconfirmedDoesNotDoubleWrap(t *testing.T) {
	if unconfirmed("outer", nil) != nil {
		t.Fatal("wrapped a nil error")
	}
	marked := unconfirmed("start container", errors.New("EOF"))
	if unconfirmed("outer", marked) != marked {
		t.Fatal("double-wrapped an existing marker")
	}
}

func TestEffectsUnconfirmedTraversesJoinsAndWrapping(t *testing.T) {
	marked := unconfirmed("update route", errors.New("EOF"))
	joined := errors.Join(errors.New("close target"), marked)
	wrapped := fmt.Errorf("deploy storefront: %w", joined)

	if !EffectsUnconfirmed(wrapped) {
		t.Fatal("marker lost through wrapping and joins")
	}
	if EffectsUnconfirmed(errors.New("unhealthy")) || EffectsUnconfirmed(nil) {
		t.Fatal("ordinary errors claimed unconfirmed effects")
	}
}

func TestMutationErrorClassifiesConservatively(t *testing.T) {
	runtime := &DockerRuntime{}

	// Only a structured known rejection proves the daemon refused the
	// mutation; its identity and message stay untouched.
	rejection := fmt.Errorf("create container: %w", errdefs.ErrNotFound.WithMessage("No such image"))
	classified := runtime.mutationError("create container", rejection)
	if EffectsUnconfirmed(classified) {
		t.Fatalf("known rejection marked unconfirmed: %v", classified)
	}
	if !errors.Is(classified, errdefs.ErrNotFound) || !strings.Contains(classified.Error(), "No such image") {
		t.Fatalf("rejection distorted: %v", classified)
	}

	// Unknown errors, server internal errors, and malformed success
	// responses never prove the mutation was not applied.
	unrecognized := []error{
		errors.New("weird unrecognized failure"),
		errdefs.ErrInternal.WithMessage("daemon panic"),
		fmt.Errorf("parse response body: %w", errors.New("invalid character 'o' in literal")),
	}
	for _, cause := range unrecognized {
		classified := runtime.mutationError("create container", cause)
		if !EffectsUnconfirmed(classified) {
			t.Fatalf("unrecognized mutation failure treated as definitive: %v", cause)
		}
		if !errors.Is(classified, cause) {
			t.Fatalf("cause identity lost: %v", classified)
		}
	}
}

func TestTransportLostRecognizesLostRequestResponses(t *testing.T) {
	lost := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		syscall.ECONNRESET,
		syscall.EPIPE,
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("Post \"http://docker/v1.51/containers/create\": %w", io.EOF),
		&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded},
	}
	for _, err := range lost {
		if !transportLost(err) {
			t.Fatalf("classified as definitive: %v", err)
		}
	}
}

func TestTransportLostRecognizesDefinitiveAnswers(t *testing.T) {
	definitive := []error{
		errors.New("container health check reported unhealthy"),
		errors.New("HTTP 409: conflict"),
		// The health gate's own exhausted budget is a known answer, not a
		// lost transport, so safe cleanup and rollback may proceed.
		fmt.Errorf("wait for container health: %w", errVerificationBudget),
		&net.OpError{Op: "dial", Net: "unix", Err: errors.New("no such file or directory")},
	}
	for _, err := range definitive {
		if transportLost(err) {
			t.Fatalf("classified as lost transport: %v", err)
		}
	}
}

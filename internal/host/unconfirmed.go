package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

	"github.com/containerd/errdefs"
)

// UnconfirmedError marks a failure whose remote effects cannot be
// established: a mutating request may have been applied, or the resulting
// final state could not be read back, so no remote cleanup may run on its
// account. It wraps the original failure, preserving its identity, and is
// never another execution state.
type UnconfirmedError struct {
	Operation string
	Cause     error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("%s: remote effects unconfirmed: %v", e.Operation, e.Cause)
}

func (e *UnconfirmedError) Unwrap() error {
	return e.Cause
}

// EffectsUnconfirmed reports whether err, or anything it wraps, means a
// mutation's remote acceptance or the resulting final state could not be
// established and the target needs manual inspection before retrying.
func EffectsUnconfirmed(err error) bool {
	var unconfirmed *UnconfirmedError

	return errors.As(err, &unconfirmed)
}

// unconfirmed marks err as leaving remote effects unverifiable, without
// double-wrapping an existing marker.
func unconfirmed(operation string, err error) error {
	if err == nil || EffectsUnconfirmed(err) {
		return err
	}

	return &UnconfirmedError{Operation: operation, Cause: err}
}

// knownRejection reports whether err is a structured answer proving the
// Docker daemon refused the mutation: the SDK maps definitive
// client-class HTTP rejections onto errdefs sentinels. Server internal
// errors, generic connection failures, and malformed or unrecognized
// responses prove nothing about what the remote applied.
func knownRejection(err error) bool {
	return errdefs.IsInvalidArgument(err) ||
		errdefs.IsNotFound(err) ||
		errdefs.IsConflict(err) ||
		errdefs.IsAlreadyExists(err) ||
		errdefs.IsFailedPrecondition(err) ||
		errdefs.IsUnauthorized(err) ||
		errdefs.IsPermissionDenied(err)
}

// transportLost reports whether err means a request or its response was
// lost in transit — EOF, a reset or closed connection, a timeout while
// waiting for the answer, or context cancellation — rather than a
// definitive remote answer. The Docker SDK's connection-failed wrapping
// covers both pre-request dial failures and lost in-flight responses, so
// the whole chain is scanned: a dial failure carries no lost-transport
// sentinel and stays definitive, while a dropped response does. Any
// residual doubt resolves conservatively toward lost transport.
func transportLost(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return false
}

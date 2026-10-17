package resilience

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	fleeterrors "github.com/baditaflorin/go-common/errors"
	"github.com/baditaflorin/go-common/retry"
)

// RetryableStatus reports whether an upstream HTTP status is worth retrying: the
// upstream (or something in front of it) is overloaded or restarting, and a later
// attempt may succeed. 4xx other than 408, 425 and 429 mean the request itself is
// wrong, and 501 and 505 will not change.
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	case http.StatusNotImplemented, http.StatusHTTPVersionNotSupported:
		return false
	}
	return code >= 500 && code <= 599
}

// Retryable reports whether err is the kind of failure a retry can fix. It asks,
// in order:
//
//  1. the error itself: any error in the chain with a `Retryable() bool` method
//     decides (meshclient's errors implement it, and so can yours);
//  2. retry.Transient, the explicit marker;
//  3. a fleet *errors.Error, by its HTTP status (RetryableStatus);
//  4. the network: timeouts, refused or reset connections, an unexpected EOF.
//
// Cancellation is never retryable. context.DeadlineExceeded is: a per-attempt
// timeout looks the same, and the retry loop stops on its own once the caller's
// context is done. Anything unrecognised is not retryable, because retrying an
// unknown failure is how a bug becomes an outage.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var r interface{ Retryable() bool }
	if errors.As(err, &r) {
		return r.Retryable()
	}
	if retry.IsTransient(err) {
		return true
	}
	var fe *fleeterrors.Error
	if errors.As(err, &fe) {
		return RetryableStatus(fe.HTTPStatus())
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

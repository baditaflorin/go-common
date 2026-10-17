package meshclient

import (
	"errors"
	"fmt"

	"github.com/baditaflorin/go-common/resilience"
)

// ErrUnavailable is what errors.Is matches for every failure that means the
// sibling is not usable right now. See UnavailableError.
var ErrUnavailable = errors.New("meshclient: upstream unavailable")

// StatusError is the sibling's answer when it was not a success. The body is the
// first MaxBodyBytes of the reply; it is not included in Error, because it may
// carry data that should not reach a log.
type StatusError struct {
	Name   string
	Method string
	Path   string
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("meshclient: %s: %s %s returned status %d", e.Name, e.Method, e.Path, e.Status)
}

// Retryable reports whether the status is one a later attempt may fix.
func (e *StatusError) Retryable() bool { return resilience.RetryableStatus(e.Status) }

// UnavailableError wraps a failure that means the sibling cannot be used right
// now. Class is the degraded-token suffix: "down", "timeout", "rate_limited" or
// "degraded". Cause is the underlying error (a *StatusError for a 5xx or 429).
type UnavailableError struct {
	Name  string
	Class string
	Cause error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("meshclient: %s unavailable (%s): %v", e.Name, e.Class, e.Cause)
}

func (e *UnavailableError) Unwrap() error { return e.Cause }

// Is makes errors.Is(err, ErrUnavailable) true.
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Retryable reports whether the cause is worth retrying, for callers that wrap
// this client in a policy of their own.
func (e *UnavailableError) Retryable() bool { return resilience.Retryable(e.Cause) }

// IsUnavailable reports whether err means the sibling is not usable right now.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// decodeError is a reply that cannot be used: too large, empty or not valid JSON.
// A sibling that answers 200 with garbage is as unusable as one that is down, but
// repeating the request will not change what it says, so it is not retried.
type decodeError struct {
	name  string
	cause error
}

func (e *decodeError) Error() string {
	return fmt.Sprintf("meshclient: %s: unusable response: %v", e.name, e.cause)
}
func (e *decodeError) Unwrap() error   { return e.cause }
func (e *decodeError) Retryable() bool { return false }

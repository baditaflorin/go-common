// Package errors defines a typed Error value that carries an HTTP
// status code, a stable machine-readable error code, a human message,
// and an optional cause. It integrates with response.Envelope so
// handlers return a consistent JSON shape without a per-handler switch.
//
// Design goals:
//   - errors.Is / errors.As compatible (Unwrap chain preserved).
//   - Machine code is a dotted string, e.g. "auth.scope_mismatch".
//     Clients should switch on Code, not on Status or Msg.
//   - HTTP status lives on the error so response helpers can map it
//     automatically.
//
// Usage:
//
//	return errors.New(http.StatusForbidden, "auth.scope_mismatch",
//	    "token scope does not cover this resource")
//
//	// wrap an existing error:
//	return errors.Wrap(err, http.StatusBadGateway, "upstream.timeout",
//	    "upstream service did not respond in time")
//
//	// check in a handler:
//	var apiErr *errors.Error
//	if errors.As(err, &apiErr) {
//	    http.Error(w, apiErr.Msg, apiErr.Status)
//	}
package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a typed fleet error. It satisfies the standard error interface
// and is compatible with errors.Is / errors.As / errors.Unwrap.
type Error struct {
	// Code is a stable, dotted, machine-readable identifier.
	// Clients should match on this, not on Msg.
	// Convention: "<domain>.<snake_case>" e.g. "auth.token_missing"
	Code string

	// Status is the HTTP status code this error maps to.
	// Defaults to 500 if not set.
	Status int

	// Msg is a human-readable description, safe to surface in API responses.
	Msg string

	// Cause wraps the underlying error (accessible via errors.Unwrap).
	Cause error
}

// New creates a new *Error with the given status, code, and message.
func New(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Msg: msg}
}

// Newf creates a new *Error with a formatted message.
func Newf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap creates a new *Error wrapping cause. If cause is already an
// *Error its Code is used when code is empty.
func Wrap(cause error, status int, code, msg string) *Error {
	if code == "" {
		var e *Error
		if errors.As(cause, &e) {
			code = e.Code
		}
	}
	return &Error{Status: status, Code: code, Msg: msg, Cause: cause}
}

// Wrapf creates a new *Error wrapping cause with a formatted message.
func Wrapf(cause error, status int, code, format string, args ...any) *Error {
	return Wrap(cause, status, code, fmt.Sprintf(format, args...))
}

// Error implements the error interface. The string includes the code and
// message; the HTTP status is omitted to avoid leaking transport concerns
// into logs.
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// Unwrap returns the wrapped cause for errors.Is / errors.As chains.
func (e *Error) Unwrap() error { return e.Cause }

// HTTPStatus returns e.Status, or 500 if it was not set.
func (e *Error) HTTPStatus() int {
	if e.Status == 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

// Is reports whether target is an *Error with the same Code.
// This lets callers use errors.Is(err, ErrAuthMissing) when ErrAuthMissing
// is a sentinel *Error.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// ─── Sentinel errors ──────────────────────────────────────────────────────

// Pre-built sentinels for the most common fleet error codes. Services
// can define their own with the same pattern.

var (
	ErrNotFound     = New(http.StatusNotFound, "not_found", "resource not found")
	ErrBadRequest   = New(http.StatusBadRequest, "bad_request", "invalid request")
	ErrUnauthorized = New(http.StatusUnauthorized, "auth.missing", "authentication required")
	ErrForbidden    = New(http.StatusForbidden, "auth.forbidden", "access denied")
	ErrConflict     = New(http.StatusConflict, "conflict", "resource already exists")
	ErrInternal     = New(http.StatusInternalServerError, "internal", "internal server error")
	ErrUnavailable  = New(http.StatusServiceUnavailable, "unavailable", "service temporarily unavailable")
	ErrTimeout      = New(http.StatusGatewayTimeout, "timeout", "upstream request timed out")
)

// ─── stdlib errors re-exports ─────────────────────────────────────────────
// Re-export the stdlib functions so callers can use a single import.

// As wraps errors.As.
func As(err error, target any) bool { return errors.As(err, target) }

// Is wraps errors.Is.
func Is(err, target error) bool { return errors.Is(err, target) }

// Unwrap wraps errors.Unwrap.
func Unwrap(err error) error { return errors.Unwrap(err) }

// FromError extracts an *Error from err. Returns the error and true if
// err (or any cause in its chain) is *Error. Returns a synthetic 500
// *Error and false otherwise.
func FromError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return &Error{
		Status: http.StatusInternalServerError,
		Code:   "internal",
		Msg:    err.Error(),
		Cause:  err,
	}, false
}

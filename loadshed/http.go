package loadshed

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/baditaflorin/go-common/response"
)

// DefaultRetryAfter is the Retry-After value (seconds) advertised on a
// shed response when the caller passes 0. One second: long enough to
// let the in-flight burst drain, short enough that a legitimate caller
// retrying loses little.
const DefaultRetryAfter = 1

// ShedErrorCode is the stable machine-readable error_code on a shed
// response envelope. Clients switch on this to distinguish "retry, I'm
// momentarily saturated" from a hard failure.
const ShedErrorCode = "load_shed"

// WriteShed writes the canonical fast-503 load-shed response: a
// Retry-After header and a fleet error envelope carrying error_code
// "load_shed". retryAfterSeconds <= 0 uses DefaultRetryAfter; an empty
// msg uses a generic default. This is the only place the shed wire
// shape is defined, so every fleet service sheds identically.
func WriteShed(w http.ResponseWriter, retryAfterSeconds int, msg string) {
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = DefaultRetryAfter
	}
	if msg == "" {
		msg = "service at capacity; retry shortly"
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(
		response.NewError(http.StatusServiceUnavailable, ShedErrorCode, msg),
	)
}

// Guard returns net/http middleware that gates every request through g:
// a request that obtains a slot runs next (releasing on return); a
// request that finds the gate full is shed via WriteShed and never
// reaches next. retryAfter and msg customise the shed response
// (0/empty => defaults).
//
// Use Guard for the simple "bound this whole endpoint" case. When you
// need to gate only a sub-path of a handler — e.g. only cache MISSES,
// letting hits and cheap direct calls through — call Gate.TryAcquire
// in-line instead.
func (g *Gate) Guard(retryAfter int, msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			release, ok := g.TryAcquire()
			if !ok {
				WriteShed(w, retryAfter, msg)
				return
			}
			defer release()
			next.ServeHTTP(w, r)
		})
	}
}

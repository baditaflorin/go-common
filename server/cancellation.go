package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
)

// CancellationHeader is the canonical request-correlation header. The
// fleet already uses X-Request-Id for log correlation (see
// middleware.RequestID); the cancellation registry piggybacks on that
// header so callers don't have to attach two different IDs to the same
// request.
const CancellationHeader = "X-Request-Id"

// CancelPathPrefix is the URL prefix the cancel handler is mounted at.
// DELETE <CancelPathPrefix><id> cancels the in-flight request whose
// X-Request-Id equals <id>.
const CancelPathPrefix = "/cancel/"

// DefaultMaxInFlight bounds the in-memory registry. New requests that
// arrive when the registry is full run normally — they just don't get
// registered for cancellation. Graceful degradation, not a 503.
const DefaultMaxInFlight = 10000

// AdminGate decides whether an inbound request to DELETE /cancel/<id>
// is permitted. Return nil to allow, or an error to deny (in which
// case the handler responds 401). Pass a nil AdminGate (the zero
// CancellationConfig default) to disable auth on the cancel endpoint
// entirely — appropriate for services on a private mesh where the
// upstream gateway already gates admin scope.
//
// A typical implementation checks a header against a shared secret:
//
//	gate := func(r *http.Request) error {
//	    if r.Header.Get(header.AdminToken) != os.Getenv("ADMIN_TOKEN") {
//	        return errors.New("forbidden")
//	    }
//	    return nil
//	}
type AdminGate func(r *http.Request) error

// CancellationConfig tunes the cancellation registry. Zero value is
// safe: MaxInFlight defaults to DefaultMaxInFlight; AdminGate nil
// means no auth (see WithCancellationRegistry doc for the rationale).
type CancellationConfig struct {
	// MaxInFlight bounds the in-memory registry. <=0 means
	// DefaultMaxInFlight.
	MaxInFlight int

	// AdminGate, when non-nil, is invoked on every DELETE /cancel/<id>
	// request. Returning an error responds 401 and skips the
	// cancellation. Default nil (no auth) — see decision rationale in
	// the PR body.
	AdminGate AdminGate
}

// cancellationRegistry tracks in-flight requests by X-Request-Id and
// exposes their cancel funcs. The map is mutex-guarded; entries are
// removed either when the handler returns (defer in the middleware)
// or when DELETE /cancel/<id> fires.
type cancellationRegistry struct {
	mu      sync.Mutex
	entries map[string]context.CancelFunc
	max     int
}

func newCancellationRegistry(max int) *cancellationRegistry {
	if max <= 0 {
		max = DefaultMaxInFlight
	}
	return &cancellationRegistry{
		entries: make(map[string]context.CancelFunc),
		max:     max,
	}
}

// add registers cancel under id. Returns true if registered, false if
// the registry is full (in which case the caller proceeds without
// cancellation support).
func (r *cancellationRegistry) add(id string, cancel context.CancelFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) >= r.max {
		return false
	}
	r.entries[id] = cancel
	return true
}

// remove drops id from the registry. Safe to call on an unknown id.
func (r *cancellationRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// cancel looks up id, invokes its cancel func, and removes the entry.
// Returns true on success, false if no entry exists for id.
func (r *cancellationRegistry) cancel(id string) bool {
	r.mu.Lock()
	cancel, ok := r.entries[id]
	if ok {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// size returns the current number of registered in-flight requests.
// Exists for tests and observability.
func (r *cancellationRegistry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// WithCancellationRegistry installs the cancellation primitive on the
// server:
//
//  1. A handler at DELETE /cancel/<id> that cancels the in-flight
//     request whose X-Request-Id matches <id>.
//  2. A middleware that intercepts incoming requests, registers them
//     in an in-memory map keyed by X-Request-Id, and replaces
//     r.Context with a cancellable one. On handler return the entry
//     is removed.
//
// If the incoming request has no X-Request-Id header the middleware
// generates one and writes it to the response so callers can later
// issue a cancel against it.
//
// The registry is process-local. Cross-replica cancellation is out
// of scope — callers that need it should target the specific replica
// (e.g. via a sticky session) or use a shared store (Redis pub/sub),
// neither of which lives in go-common.
//
// Typical use:
//
//	srv := server.New(cfg,
//	    server.WithCancellationRegistry(server.CancellationConfig{
//	        MaxInFlight: 5000,
//	        AdminGate:   myAdminGate,
//	    }),
//	)
//
// Pass server.CancellationConfig{} for defaults (10k entries, no
// admin gate — fine on a private mesh where the gateway already
// gates admin scope).
func WithCancellationRegistry(cfg CancellationConfig) Option {
	return func(s *Server) {
		reg := newCancellationRegistry(cfg.MaxInFlight)
		gate := cfg.AdminGate

		// 1. Mount the DELETE /cancel/<id> handler. Method-gated so a
		//    stray GET probe doesn't accidentally cancel a request.
		s.Mux.HandleFunc(CancelPathPrefix, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				w.Header().Set("Allow", http.MethodDelete)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if gate != nil {
				if err := gate(r); err != nil {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			id := strings.TrimPrefix(r.URL.Path, CancelPathPrefix)
			id = strings.TrimSpace(id)
			if id == "" {
				http.Error(w, "missing id", http.StatusBadRequest)
				return
			}
			if reg.cancel(id) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, "no in-flight request matches id", http.StatusNotFound)
		})

		// 2. Append the registration middleware. It runs after the
		//    default RequestID middleware (which is installed before
		//    user middlewares in New), so X-Request-Id is always
		//    populated on r by the time we see it. We still
		//    defensively generate one if absent, so the middleware
		//    can be used independently of WithMiddleware ordering.
		s.Middlewares = append(s.Middlewares, cancellationMiddleware(reg))
	}
}

// cancellationMiddleware wraps each request in a cancellable context
// and registers its cancel func under the request's X-Request-Id.
// On handler return the entry is removed; if DELETE /cancel/<id>
// fired during the handler's lifetime the context will already be
// cancelled and the handler can observe ctx.Done().
func cancellationMiddleware(reg *cancellationRegistry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip the cancel endpoint itself — registering it
			// would let a cancel request cancel itself (the
			// registry is keyed off X-Request-Id which the
			// caller may set on the DELETE too).
			if strings.HasPrefix(r.URL.Path, CancelPathPrefix) {
				next.ServeHTTP(w, r)
				return
			}

			id := r.Header.Get(CancellationHeader)
			if id == "" {
				id = generateCancellationID()
				r.Header.Set(CancellationHeader, id)
			}
			// Always reflect the id in the response header so the
			// caller can correlate. middleware.RequestID also sets
			// this; setting it again is harmless (same value when
			// RequestID ran first; otherwise we own the header).
			w.Header().Set(CancellationHeader, id)

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()

			if !reg.add(id, cancel) {
				// Registry full — log once per occurrence and run
				// without cancellation support. The handler still
				// gets a cancellable context (so its own
				// defer-cancel works), it's just not reachable
				// from DELETE /cancel/<id>.
				log.Printf("server/cancellation: registry full (max=%d); request %s runs without cancellation", reg.max, id)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			defer reg.remove(id)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func generateCancellationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never returns an error on supported
		// platforms, but if it ever does fall back to an empty
		// string rather than panic — the middleware degrades to
		// "no registration" gracefully.
		return ""
	}
	return hex.EncodeToString(b)
}

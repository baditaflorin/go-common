// Package degraded is the canonical "primitive X is not working right now"
// signal that every fleet service appends to a per-request slice and
// surfaces in its response envelope. Today each caller maintains its
// own `var degraded []string`; this package formalises the type so
// the value is observable (Prometheus counter via promx) and the JSON
// shape is canonical across the fleet.
//
// Design centre: zero-cost for callers that don't wire an observer.
// Sink is a thin wrapper around []string with a sync.Mutex; the
// observer is fired inline on Append. The observer is process-wide
// because Sink instances are per-request and we want fleet metrics,
// not per-request ones.
//
// Naming convention: append "<primitive>-down" or
// "<primitive>-degraded" — primitive is the upstream sibling, third-
// party host, internal cache, etc. The promx collector splits on the
// last "-" suffix to derive the label, so callers should keep the
// suffix to one of: "-down", "-degraded", "-timeout", "-rate_limited".
package degraded

import (
	"strings"
	"sync"
	"sync/atomic"
)

// Sink is a concurrency-safe accumulator of degraded-primitive tokens.
// Construct one per request, surface its Slice() in your response,
// pass &Sink to libraries that take a *[]string (safehttp's
// WithDegradedSink works against this type's exposed slice).
type Sink struct {
	mu    sync.Mutex
	items []string
}

// New returns an empty Sink.
func New() *Sink { return &Sink{} }

// Append records that primitive is degraded. Token is the full
// caller-chosen string (e.g. "html-proxy-down", "keystore-degraded").
// Idempotency is the caller's responsibility — duplicates are kept so
// callers retain visibility into repeated failures within one request.
func (s *Sink) Append(token string) {
	if s == nil || token == "" {
		return
	}
	s.mu.Lock()
	s.items = append(s.items, token)
	s.mu.Unlock()
	emit(Event{Token: token, Primitive: primitiveOf(token), Suffix: suffixOf(token)})
}

// Slice returns a copy of the recorded tokens in append order. Safe
// to call concurrently with Append.
func (s *Sink) Slice() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return nil
	}
	out := make([]string, len(s.items))
	copy(out, s.items)
	return out
}

// Has reports whether token is already in the sink. Useful for
// idempotent appenders that want exactly-once semantics.
func (s *Sink) Has(token string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.items {
		if t == token {
			return true
		}
	}
	return false
}

// AsSlicePtr exposes the underlying slice as a *[]string for
// libraries that take that shape (notably safehttp.WithDegradedSink).
// The pointer remains valid for the Sink's lifetime; appends through
// it bypass the observer, so prefer Sink.Append where you have the
// choice.
func (s *Sink) AsSlicePtr() *[]string {
	return &s.items
}

// Observer receives one event per Sink.Append. Implementations MUST
// NOT block — callbacks run inline on the request hot path. The
// canonical implementation lives in go-common/promx.
type Observer interface {
	ObserveDegraded(Event)
}

// Event is the per-Append payload handed to an Observer. Primitive is
// the part of Token before the last "-" (e.g. "html-proxy" from
// "html-proxy-down"); Suffix is what follows ("down", "degraded",
// "timeout"). When Token has no "-", Primitive==Token and Suffix=="".
type Event struct {
	Token     string
	Primitive string
	Suffix    string
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer. Pass nil to
// disable. Wired by promx.AutoWire.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer or nil.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func emit(ev Event) {
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveDegraded(ev)
	}
}

func primitiveOf(token string) string {
	if i := strings.LastIndex(token, "-"); i > 0 {
		return token[:i]
	}
	return token
}

func suffixOf(token string) string {
	if i := strings.LastIndex(token, "-"); i > 0 {
		return token[i+1:]
	}
	return ""
}

// Package circuitbreaker is a small, in-process circuit-breaker that
// every safehttp consumer's degraded[]-fallback was hand-rolling. A
// real breaker has three states (closed / open / half-open) and an
// observer hook so promx can emit fleet-wide state metrics:
//
//	fleet_circuit_state{service, upstream}   0=closed 1=open 2=half_open
//	fleet_circuit_trips_total{service, upstream}
//
// Design centre: cheap and obvious. One Breaker per (service,
// upstream) pair; the caller stamps the upstream slug at construction.
// State transitions are guarded by a single mutex — this isn't a hot
// path, the typical call rate is human-seconds.
//
// Semantics:
//
//   - closed:    requests pass through; failures increment the counter.
//                When ≥ FailureThreshold consecutive failures happen,
//                the breaker opens.
//   - open:      requests fail-fast with ErrOpen until OpenFor elapses,
//                at which point it flips to half-open.
//   - half-open: exactly one probe request is allowed through. Success
//                resets to closed; failure flips back to open.
//
// Use Allow() as a pre-check on the hot path, then Success() or
// Failure() on the outcome. The breaker neither retries nor sleeps;
// callers do.
package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// State buckets the breaker's transitions for the observer + metrics.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// String returns the lowercased state name.
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// ErrOpen is returned by Allow when the breaker has tripped open and
// the OpenFor window has not yet elapsed.
var ErrOpen = errors.New("circuitbreaker: open")

// Options configures a Breaker. Zero values fall back to sensible
// defaults at New time.
type Options struct {
	// Upstream is the slug of the dependency being guarded ("html-proxy",
	// "third-party-api"). Becomes the "upstream" label in metrics.
	Upstream string
	// FailureThreshold is the number of consecutive failures required
	// to trip from closed to open. Default 5.
	FailureThreshold int
	// OpenFor is how long the breaker stays open before allowing a
	// half-open probe. Default 30s.
	OpenFor time.Duration
}

// Breaker is a single named circuit breaker. Safe for concurrent use.
type Breaker struct {
	upstream         string
	failureThreshold int
	openFor          time.Duration

	mu              sync.Mutex
	state           State
	consecutiveFail int
	openedAt        time.Time
}

// New constructs a breaker. Defaults: 5 consecutive failures to trip,
// 30s open window.
func New(opts Options) *Breaker {
	if opts.FailureThreshold <= 0 {
		opts.FailureThreshold = 5
	}
	if opts.OpenFor <= 0 {
		opts.OpenFor = 30 * time.Second
	}
	if opts.Upstream == "" {
		opts.Upstream = "_unknown"
	}
	return &Breaker{
		upstream:         opts.Upstream,
		failureThreshold: opts.FailureThreshold,
		openFor:          opts.OpenFor,
		state:            StateClosed,
	}
}

// Upstream returns the slug this breaker guards.
func (b *Breaker) Upstream() string { return b.upstream }

// State returns the current state. Cheap — locks once.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow returns nil if a request is allowed through, ErrOpen if the
// breaker is open. When called in the open state and the open window
// has elapsed, Allow transitions to half-open and returns nil
// (allowing exactly one probe).
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return nil
	case StateHalfOpen:
		// The first caller to land here while half-open is the probe.
		// Subsequent concurrent callers are blocked until the probe
		// resolves and either resets to closed or trips back to open.
		// Implementation: flip to open until the probe finishes.
		b.transitionLocked(StateOpen, "probe_in_flight")
		b.openedAt = time.Now()
		return nil
	default: // StateOpen
		if time.Since(b.openedAt) >= b.openFor {
			b.transitionLocked(StateHalfOpen, "open_window_elapsed")
			return nil
		}
		return ErrOpen
	}
}

// Success records a successful call. From half-open this closes the
// breaker; from closed it resets the failure counter; from open it's
// a no-op (the probe path goes through half-open).
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateHalfOpen:
		b.transitionLocked(StateClosed, "probe_ok")
		b.consecutiveFail = 0
	case StateClosed:
		b.consecutiveFail = 0
	}
}

// Failure records a failed call. From closed, FailureThreshold
// consecutive failures trip the breaker open. From half-open, a
// single failure trips back to open.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		b.consecutiveFail++
		if b.consecutiveFail >= b.failureThreshold {
			b.openedAt = time.Now()
			b.transitionLocked(StateOpen, "threshold_reached")
		}
	case StateHalfOpen:
		b.openedAt = time.Now()
		b.transitionLocked(StateOpen, "probe_failed")
	}
}

func (b *Breaker) transitionLocked(to State, reason string) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveCircuit(Event{
			Upstream: b.upstream,
			From:     from,
			To:       to,
			Reason:   reason,
		})
	}
}

// Observer receives one event per state transition. Implementations
// MUST NOT block. The canonical implementation lives in
// go-common/promx and records state gauges + transition counters.
type Observer interface {
	ObserveCircuit(Event)
}

// Event is the per-transition payload handed to an Observer.
type Event struct {
	Upstream string
	From     State
	To       State
	Reason   string
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

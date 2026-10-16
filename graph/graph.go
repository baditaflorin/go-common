package graph

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const shutdownFlushTimeout = 5 * time.Second

// Package-level state. Initialised lazily on the first Init call (or
// the first Record if Init was never called).
var (
	stateMu   sync.RWMutex
	state     *pkgState
	stateOnce sync.Once

	// invalidIdentityLogOnce keeps the invalid-GRAPH_SERVICE_ID warning to one
	// line per process however many times Init runs.
	invalidIdentityLogOnce sync.Once
)

type pkgState struct {
	cfg       config
	serviceID string
	// identified is true once Init (or GRAPH_SERVICE_ID at bootstrap) has
	// decided the identity, including the fail-closed decision for an invalid
	// override (serviceID "" with identified true). While it is false the
	// identity is only not yet known: events are held without an owner and
	// stamped when they are sent.
	identified bool
	version    string
	ring       *ring
	sender     *sender
	counters   *atomicCounters
	rng        *rand.Rand
	rngMu      sync.Mutex
}

// Init configures the package with the calling service's identity.
// Called from server.Run / server.New so every fleet service is
// automatically wired. Safe to call once; subsequent calls update both
// event and sender identity without restarting the sender.
//
// If the package was already initialised (e.g. via an earlier Record
// from a probe) the existing ring and sender are preserved.
func Init(serviceID, version string) {
	serviceID, overrideSet := configuredServiceID(serviceID)
	stateOnce.Do(func() {
		stateMu.Lock()
		state = bootstrap(serviceID, version, serviceID != "" || overrideSet)
		stateMu.Unlock()
	})
	stateMu.Lock()
	s := state
	if state != nil {
		if serviceID != "" || overrideSet {
			state.serviceID = serviceID
			state.identified = true
		}
		if version != "" {
			state.version = version
		}
		if state.sender != nil {
			state.sender.setIdentity(state.serviceID, state.version)
		}
	}
	stateMu.Unlock()
	if s != nil && serviceID == "" && overrideSet {
		warnInvalidServiceID(s.cfg)
	}
}

// warnInvalidServiceID logs, once per process, that an invalid GRAPH_SERVICE_ID
// has switched Graph emission off. It runs from Init and from the bootstrap
// Record triggers before Init, never from Record itself, so the hot path does
// not log. It stays silent when Graph is not configured to emit anyway, where
// a bad identity changes nothing. It names the variable only: no key or
// secret, and not the rejected value either.
func warnInvalidServiceID(cfg config) {
	if !cfg.eventEmissionConfigured() {
		return
	}
	invalidIdentityLogOnce.Do(func() {
		log.Printf("graph: GRAPH_SERVICE_ID is set but is not a valid lowercase DNS label " +
			"([a-z0-9-], at most 63 characters, no leading or trailing '-'): Fleet Graph event emission is OFF " +
			"for this process and every event is dropped (counted in fleet_graph_events_dropped_invalid_identity_total); " +
			"fix or unset GRAPH_SERVICE_ID")
	})
}

// ensureInit makes Record safe to call before Init. Until an identity is
// known (GRAPH_SERVICE_ID, resolved here, or a later Init) events are held in
// the ring without a caller/target and the sender does not ship them; once the
// identity lands they are stamped with it. A placeholder identity must never
// reach the collector: it rejects a whole batch whose events do not belong to
// the authenticated writer.
func ensureInit() *pkgState {
	stateMu.RLock()
	s := state
	stateMu.RUnlock()
	if s != nil {
		return s
	}
	stateOnce.Do(func() {
		stateMu.Lock()
		serviceID, overrideSet := configuredServiceID("")
		state = bootstrap(serviceID, "", serviceID != "" || overrideSet)
		cfg := state.cfg
		stateMu.Unlock()
		if serviceID == "" && overrideSet {
			warnInvalidServiceID(cfg)
		}
	})
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}

func bootstrap(serviceID, version string, identified bool) *pkgState {
	cfg := loadConfig()
	counters := &atomicCounters{}
	r := newRing(cfg.bufferSize)
	s := &pkgState{
		cfg:        cfg,
		serviceID:  serviceID,
		identified: identified,
		version:    version,
		ring:       r,
		counters:   counters,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.sender = newSender(cfg, serviceID, version, r, counters)
	go s.sender.run()
	return s
}

// Record adds an event to the ring. Never blocks; if the ring is full
// the oldest event is dropped to make room.
func Record(e Event) {
	s := ensureInit()
	if !s.cfg.eventEmissionConfigured() {
		return
	}
	stateMu.RLock()
	serviceID, identified := s.serviceID, s.identified
	stateMu.RUnlock()
	if serviceID == "" && identified {
		// Invalid GRAPH_SERVICE_ID: fail closed, but visibly. One atomic add,
		// no logging here (see warnInvalidServiceID).
		atomic.AddInt64(&s.counters.EventsDroppedInvalidIdentity, 1)
		return
	}
	// Sampling: roll once per event. EventsSampled counts the *kept*
	// after-sampling events (so it equals EventsRecorded at rate 1.0).
	if s.cfg.sampleRate < 1.0 {
		s.rngMu.Lock()
		keep := s.rng.Float64() < s.cfg.sampleRate
		s.rngMu.Unlock()
		if !keep {
			atomic.AddInt64(&s.counters.EventsSampled, 0)
			return
		}
	}
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().UnixNano()
	}
	// Fill in caller from package identity if the caller didn't set
	// it (the safehttp transport always knows itself; the inbound
	// middleware infers caller from User-Agent). Before the identity is
	// known it stays blank and the sender stamps it (see stampOwnIdentity).
	if e.Direction == "out" && e.Caller == "" {
		e.Caller = serviceID
	}
	if e.Direction == "in" && e.Target == "" {
		e.Target = serviceID
	}
	_, dropped := s.ring.push(e)
	atomic.AddInt64(&s.counters.EventsRecorded, 1)
	atomic.AddInt64(&s.counters.EventsSampled, 1)
	if dropped {
		atomic.AddInt64(&s.counters.EventsDropped, 1)
	}
}

// Stats returns a snapshot of the package counters. Suitable for
// exposing via /metrics; never blocks more than one atomic load each.
func Stats() Counters {
	s := ensureInit()
	return s.counters.snapshot()
}

// ServiceID returns the configured identity, useful for callers
// that want to tag custom events. It is empty until Init (or
// GRAPH_SERVICE_ID) has supplied one; an Event left without Caller/Target is
// stamped with the identity when it is sent.
func ServiceID() string {
	_ = ensureInit()
	stateMu.RLock()
	defer stateMu.RUnlock()
	if state == nil {
		return ""
	}
	return state.serviceID
}

// Enabled reports whether event emission is on. Useful in tests and
// in hot paths that want to skip Event allocation.
func Enabled() bool {
	s := ensureInit()
	return !s.identityInvalid() && s.cfg.eventEmissionEnabled()
}

// identityInvalid reports the fail-closed state: GRAPH_SERVICE_ID is set but
// is not a valid label, so no event may be attributed to any service.
func (s *pkgState) identityInvalid() bool {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return s.serviceID == "" && s.identified
}

// observing is the per-call check of the instrumentation chokepoints
// (RoundTripper, Middleware). It answers like Enabled and additionally counts
// the call as a dropped event when the only thing stopping emission is an
// invalid GRAPH_SERVICE_ID: those chokepoints return before Record, so
// without this the counter would stay at zero for the services it exists for.
// Enabled itself stays a side-effect-free query.
func observing() bool {
	s := ensureInit()
	if s.identityInvalid() {
		if s.cfg.eventEmissionConfigured() {
			atomic.AddInt64(&s.counters.EventsDroppedInvalidIdentity, 1)
		}
		return false
	}
	return s.cfg.eventEmissionEnabled()
}

// Shutdown stops the background sender and makes a final best-effort flush.
// The optional caller context can shorten the five-second maximum; omitting
// it preserves compatibility with existing callers while remaining bounded.
func Shutdown(contexts ...context.Context) error {
	if len(contexts) > 1 {
		return errors.New("graph shutdown accepts at most one context")
	}
	parent := context.Background()
	if len(contexts) == 1 && contexts[0] != nil {
		parent = contexts[0]
	}
	ctx, cancel := context.WithTimeout(parent, shutdownFlushTimeout)
	defer cancel()

	stateMu.RLock()
	s := state
	stateMu.RUnlock()
	if s == nil || s.sender == nil {
		return nil
	}
	return s.sender.shutdown(ctx)
}

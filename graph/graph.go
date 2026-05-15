package graph

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Package-level state. Initialised lazily on the first Init call (or
// the first Record if Init was never called).
var (
	stateMu   sync.RWMutex
	state     *pkgState
	stateOnce sync.Once
)

type pkgState struct {
	cfg       config
	serviceID string
	version   string
	ring      *ring
	sender    *sender
	counters  *atomicCounters
	rng       *rand.Rand
	rngMu     sync.Mutex
}

// Init configures the package with the calling service's identity.
// Called from server.Run / server.New so every fleet service is
// automatically wired. Safe to call once; subsequent calls update
// identity but do not restart the sender.
//
// If the package was already initialised (e.g. via an earlier Record
// from a probe) the existing ring and sender are preserved.
func Init(serviceID, version string) {
	stateOnce.Do(func() {
		stateMu.Lock()
		state = bootstrap(serviceID, version)
		stateMu.Unlock()
	})
	stateMu.Lock()
	if state != nil {
		if serviceID != "" {
			state.serviceID = serviceID
		}
		if version != "" {
			state.version = version
		}
	}
	stateMu.Unlock()
}

// ensureInit makes Record safe to call before Init. The fallback
// identity is "unknown"; once a later Init lands, future events get
// the right slug.
func ensureInit() *pkgState {
	stateMu.RLock()
	s := state
	stateMu.RUnlock()
	if s != nil {
		return s
	}
	stateOnce.Do(func() {
		stateMu.Lock()
		state = bootstrap("unknown", "")
		stateMu.Unlock()
	})
	stateMu.RLock()
	defer stateMu.RUnlock()
	return state
}

func bootstrap(serviceID, version string) *pkgState {
	cfg := loadConfig()
	counters := &atomicCounters{}
	r := newRing(cfg.bufferSize)
	s := &pkgState{
		cfg:       cfg,
		serviceID: serviceID,
		version:   version,
		ring:      r,
		counters:  counters,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.sender = newSender(cfg, serviceID, version, r, counters)
	go s.sender.run()
	return s
}

// Record adds an event to the ring. Never blocks; if the ring is full
// the oldest event is dropped to make room.
func Record(e Event) {
	s := ensureInit()
	if !s.cfg.enabled {
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
	// middleware infers caller from User-Agent).
	if e.Direction == "out" && e.Caller == "" {
		e.Caller = s.serviceID
	}
	if e.Direction == "in" && e.Target == "" {
		e.Target = s.serviceID
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
// that want to tag custom events.
func ServiceID() string {
	s := ensureInit()
	return s.serviceID
}

// Enabled reports whether event emission is on. Useful in tests and
// in hot paths that want to skip Event allocation.
func Enabled() bool {
	s := ensureInit()
	return s.cfg.enabled
}

// Shutdown stops the background sender and flushes pending events.
// Intended for tests and graceful-shutdown paths; normal services
// run for the life of the process.
func Shutdown() {
	stateMu.RLock()
	s := state
	stateMu.RUnlock()
	if s == nil || s.sender == nil {
		return
	}
	s.sender.shutdown()
}

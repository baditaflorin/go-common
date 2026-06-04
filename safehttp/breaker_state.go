package safehttp

import (
	"net/http"
	"sync"
)

// Persistent-breaker-state proposal F1 (fleet 30-list).
//
// safehttp's BackoffCoordinator client (see extras.go) keeps a small
// in-memory map of "hosts I just saw fail" so it knows whether to
// consult the coordinator on the next call. The map lives only in
// extrasTransport.hostState, which means a deploy or container
// restart loses it — and a host that has been flapping for the past
// hour has to be relearned from scratch (one fresh 5xx per host
// before the coordinator gets a chance to gate).
//
// This file adds optional disk-backed persistence for that local
// last-known-bad list. It is strictly best-effort:
//
//   - File missing at startup = empty state, no error.
//   - File unreadable / malformed / wrong version = warn + empty.
//   - Write failure = warn + continue (never crashes the service).
//   - Cross-replica consistency is NOT addressed — each instance
//     has its own file. The coordinator is the authoritative
//     server-side state; this is just a client-side warm cache so
//     a deploy doesn't reset learned-bad endpoints to zero.
//
// File format (version 1):
//
//	{
//	  "version": 1,
//	  "saved_at": "2026-05-19T08:00:00Z",
//	  "endpoints": [
//	    {"host":"foo.example.com","status":503,"retry_after_seconds":0,
//	     "ts":"2026-05-19T07:59:30Z"},
//	    ...
//	  ]
//	}
//
// Only hosts with a *non-trivial* state are persisted — currently
// every entry in hostState is non-trivial (we delete on success),
// but the writer enforces it again so a future change to hostState
// semantics cannot accidentally bloat the file.

// BreakerStateOption tweaks the persistent-breaker-state behaviour.
type BreakerStateOption func(*breakerStateConfig)

// newBreakerStore wires a store to a transport, warms the transport
// from disk (best-effort), and spawns the periodic-save goroutine
// (only if interval > 0; tests can disable the ticker by passing
// interval=0).
func newBreakerStore(cfg *breakerStateConfig, tr *extrasTransport) *breakerStore {
	s := &breakerStore{
		path:           cfg.path,
		interval:       cfg.persistInterval,
		saveOnShutdown: cfg.saveOnShutdown,
		tr:             tr,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	s.loadFromDisk()
	go s.run()
	return s
}

// --- registry: map *http.Client -> *breakerStore -------------------
//
// NewClient returns *http.Client (the canonical Go shape) so we
// can't add a Close method directly. Instead, NewClient registers
// the store under the returned client and ShutdownBreakerState
// looks it up. The map holds the store, not the client, so the
// client can be GC'd if the caller forgets to shut down (the
// goroutine will leak, but only one per forgotten client — not a
// per-request leak).

var (
	breakerStoreRegistryMu sync.RWMutex
	breakerStoreRegistry   = map[*http.Client]*breakerStore{}
)

func registerBreakerStore(c *http.Client, s *breakerStore) {
	if c == nil || s == nil {
		return
	}
	breakerStoreRegistryMu.Lock()
	breakerStoreRegistry[c] = s
	breakerStoreRegistryMu.Unlock()
}

// ShutdownBreakerState flushes and stops the persistent-state
// background loop for a client previously constructed with
// WithPersistentBreakerState. Idempotent. Returns nil if the
// client has no associated state (common — most callers don't
// opt in) so it's safe to `defer` unconditionally.
func ShutdownBreakerState(c *http.Client) error {
	if c == nil {
		return nil
	}
	breakerStoreRegistryMu.Lock()
	s, ok := breakerStoreRegistry[c]
	if ok {
		delete(breakerStoreRegistry, c)
	}
	breakerStoreRegistryMu.Unlock()
	if !ok {
		return nil
	}
	return s.Close()
}

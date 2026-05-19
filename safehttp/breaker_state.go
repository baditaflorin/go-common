package safehttp

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
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

const breakerStateVersion = 1

// breakerStateConfig holds the knobs for WithPersistentBreakerState.
type breakerStateConfig struct {
	path            string
	persistInterval time.Duration
	saveOnShutdown  bool
}

// BreakerStateOption tweaks the persistent-breaker-state behaviour.
type BreakerStateOption func(*breakerStateConfig)

// WithPersistInterval sets how often the breaker state is flushed
// to disk while the process is running. Default 60s. Values below
// 100ms are clamped to 100ms — a tighter loop is almost certainly
// a test artefact.
func WithPersistInterval(d time.Duration) BreakerStateOption {
	return func(c *breakerStateConfig) { c.persistInterval = d }
}

// WithSaveOnShutdown controls whether ShutdownBreakerState flushes
// once more on the way out. Default true; pass false if the caller
// owns shutdown ordering and wants to skip the final write.
func WithSaveOnShutdown(save bool) BreakerStateOption {
	return func(c *breakerStateConfig) { c.saveOnShutdown = save }
}

// WithPersistentBreakerState wires the local backoff-coordinator
// client state (the last-known-bad host list) to a disk-backed
// store at the given path. Only meaningful when paired with
// WithBackoffCoordinator — without it, there is no state to persist.
//
// At construction the file (if present) is read and the in-memory
// state is warmed with it; on a periodic ticker (default 60s) and
// on ShutdownBreakerState, the state is written atomically (write
// to "<path>.tmp" then rename) so a crash mid-write cannot corrupt
// the file.
//
// All failure modes are warnings, never fatal: persistence is
// best-effort observability, not load-bearing infrastructure.
//
// Typical use:
//
//	cli := safehttp.NewClient(
//	    safehttp.WithBackoffCoordinator(os.Getenv("BACKOFF_COORDINATOR_URL")),
//	    safehttp.WithPersistentBreakerState("/var/lib/myservice/breaker.json"),
//	)
//	defer safehttp.ShutdownBreakerState(cli)
func WithPersistentBreakerState(path string, opts ...BreakerStateOption) Option {
	cfg := breakerStateConfig{
		path:            path,
		persistInterval: 60 * time.Second,
		saveOnShutdown:  true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.persistInterval > 0 && cfg.persistInterval < 100*time.Millisecond {
		cfg.persistInterval = 100 * time.Millisecond
	}
	return func(o *options) { o.breakerState = &cfg }
}

// persistedEndpoint is the on-disk shape of a single host entry.
// Field tags are stable across versions; new fields must default to
// the v1-compatible zero value.
type persistedEndpoint struct {
	Host              string    `json:"host"`
	Status            int       `json:"status"`
	RetryAfterSeconds int       `json:"retry_after_seconds"`
	TS                time.Time `json:"ts"`
}

// persistedState is the on-disk file shape.
type persistedState struct {
	Version   int                 `json:"version"`
	SavedAt   time.Time           `json:"saved_at"`
	Endpoints []persistedEndpoint `json:"endpoints"`
}

// breakerStore manages the periodic save + shutdown flush. It is
// constructed once per *http.Client in NewClient and attached to
// the extrasTransport so the transport can be observed without an
// import cycle (the store reads the transport's host map under its
// existing mutex).
type breakerStore struct {
	path           string
	interval       time.Duration
	saveOnShutdown bool

	tr *extrasTransport

	stop    chan struct{}
	done    chan struct{}
	stopped atomic.Bool
}

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

// run is the periodic-save loop. Exits on stop; on exit, if
// saveOnShutdown is set, performs one final flush.
func (s *breakerStore) run() {
	defer close(s.done)
	if s.interval <= 0 {
		// No ticker — wait for shutdown only. Still honors
		// SaveOnShutdown for the final flush.
		<-s.stop
		if s.saveOnShutdown {
			s.saveToDisk()
		}
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.saveToDisk()
		case <-s.stop:
			if s.saveOnShutdown {
				s.saveToDisk()
			}
			return
		}
	}
}

// Close signals the save loop to exit and waits for the final
// flush (if saveOnShutdown). Idempotent; safe to call from a
// signal handler.
func (s *breakerStore) Close() error {
	if !s.stopped.CompareAndSwap(false, true) {
		return nil
	}
	close(s.stop)
	<-s.done
	return nil
}

// loadFromDisk reads the persisted file and seeds tr.hostState.
// Every failure mode warns and returns — the caller (NewClient)
// must not abort on a bad state file.
func (s *breakerStore) loadFromDisk() {
	if s.path == "" || s.tr == nil {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // missing is fine
		}
		log.Printf("safehttp: breaker state load: %v (starting empty)", err)
		return
	}
	defer f.Close()

	// Cap the read to a sane upper bound — a corrupted file should
	// not exhaust memory. 4 MiB is enough for tens of thousands of
	// host entries.
	const maxFile = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		log.Printf("safehttp: breaker state load read: %v (starting empty)", err)
		return
	}
	if len(data) > maxFile {
		log.Printf("safehttp: breaker state load: file > %dB, refusing (starting empty)", maxFile)
		return
	}

	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("safehttp: breaker state load parse: %v (starting empty)", err)
		return
	}
	if st.Version != breakerStateVersion {
		log.Printf("safehttp: breaker state load: unsupported version %d (want %d), starting empty",
			st.Version, breakerStateVersion)
		return
	}

	s.tr.hostMu.Lock()
	defer s.tr.hostMu.Unlock()
	if s.tr.hostState == nil {
		s.tr.hostState = make(map[string]hostFailure, len(st.Endpoints))
	}
	for _, e := range st.Endpoints {
		if e.Host == "" {
			continue
		}
		// TTL filter at load: if the entry is already older than
		// hostFailureTTL it's not actionable; skip rather than seed
		// a stale slot the next recentFailure call will drop anyway.
		if !e.TS.IsZero() && time.Since(e.TS) > hostFailureTTL {
			continue
		}
		s.tr.hostState[e.Host] = hostFailure{
			status:            e.Status,
			retryAfterSeconds: e.RetryAfterSeconds,
			ts:                e.TS,
		}
	}
}

// saveToDisk writes the current tr.hostState atomically (tmp file
// + rename). Write failures are logged and swallowed.
func (s *breakerStore) saveToDisk() {
	if s.path == "" || s.tr == nil {
		return
	}

	endpoints := s.snapshotEndpoints()
	st := persistedState{
		Version:   breakerStateVersion,
		SavedAt:   time.Now().UTC(),
		Endpoints: endpoints,
	}
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		log.Printf("safehttp: breaker state save marshal: %v", err)
		return
	}

	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("safehttp: breaker state save mkdir %q: %v", dir, err)
			return
		}
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("safehttp: breaker state save write %q: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("safehttp: breaker state save rename: %v", err)
		// Best-effort cleanup; ignore secondary error.
		_ = os.Remove(tmp)
		return
	}
}

// snapshotEndpoints copies the current host-failure map into a
// slice suitable for JSON encoding, filtering out trivial entries
// (no failures, expired entries). Holds the transport mutex only
// for the copy.
func (s *breakerStore) snapshotEndpoints() []persistedEndpoint {
	s.tr.hostMu.Lock()
	defer s.tr.hostMu.Unlock()
	if len(s.tr.hostState) == 0 {
		return nil
	}
	out := make([]persistedEndpoint, 0, len(s.tr.hostState))
	now := time.Now()
	for host, f := range s.tr.hostState {
		// Only persist non-trivial state. hostFailure carries a
		// non-zero status (5xx/429) OR a network error (status==0
		// + ts set). Filter out anything with no recorded ts (a
		// shape that shouldn't occur today but cheap to enforce).
		if f.ts.IsZero() {
			continue
		}
		// TTL filter: don't write entries that are already past
		// hostFailureTTL — they'll be dropped on the next
		// recentFailure call anyway.
		if now.Sub(f.ts) > hostFailureTTL {
			continue
		}
		out = append(out, persistedEndpoint{
			Host:              host,
			Status:            f.status,
			RetryAfterSeconds: f.retryAfterSeconds,
			TS:                f.ts.UTC(),
		})
	}
	return out
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


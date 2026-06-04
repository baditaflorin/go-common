package safehttp

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

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

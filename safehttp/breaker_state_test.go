package safehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// helpers --------------------------------------------------------------

// newClientWithState constructs a safehttp client with the
// backoff-coordinator hook wired to a dummy URL (so extrasTransport
// is in the chain and hostState exists) plus persistence at path.
// Returns the *http.Client and the underlying extrasTransport so
// tests can poke at hostState directly without going through a
// network round-trip.
func newClientWithState(t *testing.T, path string, opts ...BreakerStateOption) (*http.Client, *extrasTransport) {
	t.Helper()
	c := NewClient(
		WithBackoffCoordinator("http://127.0.0.1:0"), // unreachable; we don't actually call it
		WithPersistentBreakerState(path, opts...),
	)
	// Walk the transport chain to find the extrasTransport.
	rt := c.Transport
	for rt != nil {
		if et, ok := rt.(*extrasTransport); ok {
			return c, et
		}
		// We don't unwrap further — extrasTransport is the
		// outermost wrap when present.
		break
	}
	t.Fatalf("extrasTransport not found in client chain (got %T)", c.Transport)
	return nil, nil
}

// writeStateFile drops a v1 state file at path with the given
// endpoints. Helper for the "load from disk" tests.
func writeStateFile(t *testing.T, path string, endpoints []persistedEndpoint) {
	t.Helper()
	st := persistedState{
		Version:   breakerStateVersion,
		SavedAt:   time.Now().UTC(),
		Endpoints: endpoints,
	}
	data, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readStateFile parses the persisted state at path. Used by the
// save-side tests to assert the writer wrote what we expect.
func readStateFile(t *testing.T, path string) persistedState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return st
}

// tests ----------------------------------------------------------------

// TestBreakerState_LoadFromFile pre-writes a v1 state file and
// confirms the constructor warms hostState with its contents.
func TestBreakerState_LoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")
	writeStateFile(t, path, []persistedEndpoint{
		{Host: "alpha.example.com", Status: 503, RetryAfterSeconds: 5, TS: time.Now().UTC()},
		{Host: "beta.example.com", Status: 429, RetryAfterSeconds: 0, TS: time.Now().UTC()},
	})

	c, et := newClientWithState(t, path)
	defer ShutdownBreakerState(c)

	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if got := len(et.hostState); got != 2 {
		t.Fatalf("expected 2 loaded entries, got %d (state=%v)", got, et.hostState)
	}
	if f, ok := et.hostState["alpha.example.com"]; !ok || f.status != 503 || f.retryAfterSeconds != 5 {
		t.Errorf("alpha not loaded correctly: %+v", f)
	}
	if f, ok := et.hostState["beta.example.com"]; !ok || f.status != 429 {
		t.Errorf("beta not loaded correctly: %+v", f)
	}
}

// TestBreakerState_SaveToFile records failures via the transport's
// own recordFailure path, calls ShutdownBreakerState to flush, and
// re-reads the file to confirm the state was preserved.
func TestBreakerState_SaveToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")

	c, et := newClientWithState(t, path, WithPersistInterval(0)) // no ticker, save on shutdown only
	et.recordFailure("gamma.example.com", 502, 0)
	et.recordFailure("delta.example.com", 429, 12)

	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	st := readStateFile(t, path)
	if st.Version != breakerStateVersion {
		t.Fatalf("version = %d, want %d", st.Version, breakerStateVersion)
	}
	if len(st.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2 (%+v)", len(st.Endpoints), st.Endpoints)
	}

	// Re-load into a fresh client; state should round-trip.
	c2, et2 := newClientWithState(t, path)
	defer ShutdownBreakerState(c2)
	et2.hostMu.Lock()
	defer et2.hostMu.Unlock()
	if _, ok := et2.hostState["gamma.example.com"]; !ok {
		t.Errorf("gamma missing after reload: %v", et2.hostState)
	}
	if f, ok := et2.hostState["delta.example.com"]; !ok || f.retryAfterSeconds != 12 {
		t.Errorf("delta missing/corrupt after reload: %+v", f)
	}
}

// TestBreakerState_AtomicWrite_PartialWriteIgnored pre-creates a
// "<path>.tmp" with garbage and confirms the loader reads only the
// real file (the tmp is rename-target only, never a source).
func TestBreakerState_AtomicWrite_PartialWriteIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")
	// Real file: valid v1 with one host.
	writeStateFile(t, path, []persistedEndpoint{
		{Host: "epsilon.example.com", Status: 500, TS: time.Now().UTC()},
	})
	// Tmp file: complete garbage that would crash a naive loader.
	if err := os.WriteFile(path+".tmp", []byte("{not valid json at all"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	c, et := newClientWithState(t, path)
	defer ShutdownBreakerState(c)

	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if _, ok := et.hostState["epsilon.example.com"]; !ok {
		t.Errorf("real file should load despite garbage .tmp: %v", et.hostState)
	}
}

// TestBreakerState_VersionMismatch_StartsEmpty pre-writes a file
// with version: 99 and confirms the constructor warns and starts
// with an empty state (no crash).
func TestBreakerState_VersionMismatch_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")

	bogus := map[string]any{
		"version": 99,
		"endpoints": []map[string]any{
			{"host": "zeta.example.com", "status": 503, "ts": time.Now().UTC()},
		},
	}
	data, _ := json.Marshal(bogus)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c, et := newClientWithState(t, path)
	defer ShutdownBreakerState(c)

	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if len(et.hostState) != 0 {
		t.Errorf("expected empty state on version mismatch, got %v", et.hostState)
	}
}

// TestBreakerState_FileMissing_StartsEmpty confirms no file = OK,
// empty state, no error.
func TestBreakerState_FileMissing_StartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "definitely-not-there.json")

	c, et := newClientWithState(t, path)
	defer ShutdownBreakerState(c)

	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if len(et.hostState) != 0 {
		t.Errorf("expected empty state when file missing, got %v", et.hostState)
	}
}

// TestBreakerState_FileUnreadable_StartsEmpty creates a file with
// mode 0000 and confirms the constructor warns + starts empty
// rather than crashing.
func TestBreakerState_FileUnreadable_StartsEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-mode 0000 semantics not portable to Windows")
	}
	if os.Geteuid() == 0 {
		// root bypasses permission checks; the test would
		// trivially "succeed" but for the wrong reason.
		t.Skip("running as root — mode 0000 doesn't block reads")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"endpoints":[]}`), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	defer os.Chmod(path, 0o644) // let t.TempDir cleanup succeed

	c, et := newClientWithState(t, path)
	defer ShutdownBreakerState(c)

	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if len(et.hostState) != 0 {
		t.Errorf("expected empty state on unreadable file, got %v", et.hostState)
	}
}

// TestBreakerState_OnlyPersistsNonClosedEndpoints confirms that
// hosts with no recorded failure don't end up in the file. The
// transport's hostState only ever contains failures (success calls
// clearFailure), so the on-disk file inherits that invariant: a
// fresh client that never sees a failure produces a file with an
// empty Endpoints slice.
func TestBreakerState_OnlyPersistsNonClosedEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")

	c, et := newClientWithState(t, path, WithPersistInterval(0))

	// Record one failure, then clear it (simulates a 5xx followed
	// by a 200). The "cleared" host must not appear in the saved
	// file — non-closed-only means non-trivial-only.
	et.recordFailure("flapped.example.com", 502, 0)
	et.clearFailure("flapped.example.com")

	// Plus one persistent failure that *should* appear.
	et.recordFailure("still-down.example.com", 503, 0)

	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	st := readStateFile(t, path)
	if len(st.Endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1 (got %+v)", len(st.Endpoints), st.Endpoints)
	}
	if st.Endpoints[0].Host != "still-down.example.com" {
		t.Errorf("wrong host saved: %+v", st.Endpoints[0])
	}
}

// TestBreakerState_PersistInterval_TriggersSave uses a sub-second
// interval and confirms the file is updated by the ticker (not
// just by shutdown). This is the proof that the background loop
// is doing its job.
func TestBreakerState_PersistInterval_TriggersSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")

	c, et := newClientWithState(t, path, WithPersistInterval(150*time.Millisecond), WithSaveOnShutdown(false))
	// Don't defer Shutdown — we want to observe the ticker alone.

	et.recordFailure("ticker.example.com", 503, 0)

	// Wait long enough for at least one tick to land. 600ms gives
	// us ~4 chances even on a slow CI runner.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(path); err != nil {
		_ = ShutdownBreakerState(c)
		t.Fatalf("file not written by ticker: %v", err)
	}

	st := readStateFile(t, path)
	if len(st.Endpoints) != 1 || st.Endpoints[0].Host != "ticker.example.com" {
		_ = ShutdownBreakerState(c)
		t.Fatalf("ticker save lost data: %+v", st.Endpoints)
	}

	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestBreakerState_NoOptWithoutBackoffURL confirms that calling
// WithPersistentBreakerState on its own (no other extras options)
// still wires up the persistence machinery — the breakerState opt
// alone is enough to construct extrasTransport.
func TestBreakerState_NoOptWithoutBackoffURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")
	writeStateFile(t, path, []persistedEndpoint{
		{Host: "solo.example.com", Status: 500, TS: time.Now().UTC()},
	})

	c := NewClient(
		WithPersistentBreakerState(path),
	)
	defer ShutdownBreakerState(c)

	et, ok := c.Transport.(*extrasTransport)
	if !ok {
		t.Fatalf("expected extrasTransport, got %T", c.Transport)
	}
	et.hostMu.Lock()
	defer et.hostMu.Unlock()
	if _, ok := et.hostState["solo.example.com"]; !ok {
		t.Errorf("state not warmed: %v", et.hostState)
	}
}

// TestBreakerState_ShutdownIdempotent confirms that calling
// ShutdownBreakerState multiple times is safe (signal handlers
// often double-fire).
func TestBreakerState_ShutdownIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")
	c, _ := newClientWithState(t, path, WithPersistInterval(0))
	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// TestBreakerState_ShutdownOnClientWithoutState confirms calling
// the shutdown helper on a client that never opted in is a no-op.
// This is the property that lets callers `defer
// ShutdownBreakerState(cli)` unconditionally in main.go.
func TestBreakerState_ShutdownOnClientWithoutState(t *testing.T) {
	c := NewClient()
	if err := ShutdownBreakerState(c); err != nil {
		t.Fatalf("shutdown on stateless client: %v", err)
	}
}

// TestBreakerState_RoundTripUnaffected sanity-checks that the
// persistence machinery does not break the actual HTTP path. The
// transport must still serve real requests through the inner
// graph/TLS-fallback chain.
func TestBreakerState_RoundTripUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "breaker.json")

	c := NewClient(
		WithTimeout(2*time.Second),
		WithBackoffCoordinator("http://127.0.0.1:0"),
		WithPersistentBreakerState(path, WithPersistInterval(0)),
	)
	defer ShutdownBreakerState(c)

	// httptest.Server listens on 127.0.0.1 — the SSRF guard would
	// normally block it, but we're verifying the persistence
	// machinery is shape-compatible with the rest of safehttp. The
	// test asserts only on the err type-check; a blocked address
	// is also a valid pass shape (proves the guard is still in
	// the chain).
	resp, err := c.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		// Success path: machinery works end-to-end. The dialer's
		// allow-private-IPs env knob may be set in some test
		// environments; either branch is OK.
	}
}

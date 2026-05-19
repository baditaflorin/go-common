package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler returns an http.Handler that increments a counter
// on every invocation. Useful for verifying a handler is or is not
// re-invoked across retries.
func countingHandler(count *int32, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func doReq(t *testing.T, h http.Handler, method, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", nil)
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestIdempotency_CacheHitReplaysResponse(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{})(countingHandler(&count, http.StatusCreated, `{"id":1}`))

	w1 := doReq(t, h, http.MethodPost, "abc")
	if w1.Code != http.StatusCreated || w1.Body.String() != `{"id":1}` {
		t.Fatalf("first call: got status=%d body=%q", w1.Code, w1.Body.String())
	}
	w2 := doReq(t, h, http.MethodPost, "abc")
	if w2.Code != http.StatusCreated || w2.Body.String() != `{"id":1}` {
		t.Fatalf("second call: got status=%d body=%q", w2.Code, w2.Body.String())
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("handler should run exactly once, got %d", got)
	}
	if got := w2.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type not replayed: %q", got)
	}
}

func TestIdempotency_DifferentKeysIndependentlyCache(t *testing.T) {
	var count int32
	// Echo the count as the response body so we can verify each key
	// remembers its own response.
	h := IdempotencyKey(IdempotencyConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strconv.Itoa(int(n))))
	}))

	wA1 := doReq(t, h, http.MethodPost, "A")
	wB1 := doReq(t, h, http.MethodPost, "B")
	wA2 := doReq(t, h, http.MethodPost, "A")
	wB2 := doReq(t, h, http.MethodPost, "B")

	if wA1.Body.String() != "1" || wA2.Body.String() != "1" {
		t.Fatalf("key A: want both replays to say 1, got %q / %q", wA1.Body.String(), wA2.Body.String())
	}
	if wB1.Body.String() != "2" || wB2.Body.String() != "2" {
		t.Fatalf("key B: want both replays to say 2, got %q / %q", wB1.Body.String(), wB2.Body.String())
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("handler should run once per key (2 total), got %d", got)
	}
}

func TestIdempotency_NoHeader_Passthrough(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{})(countingHandler(&count, http.StatusOK, `ok`))

	for i := 0; i < 3; i++ {
		w := doReq(t, h, http.MethodPost, "")
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status %d", i, w.Code)
		}
		if w.Header().Get(IdempotencyCachedHeader) != "" {
			t.Fatalf("call %d: unexpected Idempotency-Cached header on passthrough", i)
		}
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("handler should run on every call without header, got %d", got)
	}
}

func TestIdempotency_GETMethodSkipped(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{})(countingHandler(&count, http.StatusOK, `ok`))

	for i := 0; i < 3; i++ {
		w := doReq(t, h, http.MethodGet, "shared")
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status %d", i, w.Code)
		}
		if w.Header().Get(IdempotencyCachedHeader) != "" {
			t.Fatalf("call %d: GET with key should not be cached", i)
		}
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("GET should not consult the cache, want 3 invocations, got %d", got)
	}
}

func TestIdempotency_5xxResponseNotCached(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&count, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))

	w1 := doReq(t, h, http.MethodPost, "retry-me")
	w2 := doReq(t, h, http.MethodPost, "retry-me")

	if w1.Code != http.StatusInternalServerError || w2.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500/500, got %d/%d", w1.Code, w2.Code)
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("5xx must not be cached — handler should run twice, got %d", got)
	}
	if w2.Header().Get(IdempotencyCachedHeader) != "" {
		t.Fatalf("5xx replay should not be marked cached")
	}
}

func TestIdempotency_TTLExpiry(t *testing.T) {
	var count int32
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }

	h := IdempotencyKey(IdempotencyConfig{
		TTL: 5 * time.Second,
		now: now,
	})(countingHandler(&count, http.StatusOK, `v`))

	doReq(t, h, http.MethodPost, "k")
	doReq(t, h, http.MethodPost, "k") // hit (within TTL)
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("within TTL: want 1 invocation, got %d", got)
	}

	// Advance past TTL.
	clock = clock.Add(10 * time.Second)
	doReq(t, h, http.MethodPost, "k")
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("after TTL: want 2 invocations, got %d", got)
	}
}

func TestIdempotency_MaxEntriesLRU(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{
		MaxEntries: 2,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Header.Get(IdempotencyKeyHeader)))
	}))

	// Fill the cache: A, B.
	doReq(t, h, http.MethodPost, "A")
	doReq(t, h, http.MethodPost, "B")
	// Insert C — evicts A (oldest, never read since insertion).
	doReq(t, h, http.MethodPost, "C")

	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("expected 3 invocations populating the cache, got %d", got)
	}

	// B and C should still be cached → no new invocations.
	wB := doReq(t, h, http.MethodPost, "B")
	wC := doReq(t, h, http.MethodPost, "C")
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("B and C should still be cached; invocations=%d", got)
	}
	if wB.Header().Get(IdempotencyCachedHeader) != "true" || wC.Header().Get(IdempotencyCachedHeader) != "true" {
		t.Fatalf("B/C replays should be marked cached")
	}

	// Re-request A — was evicted, so handler runs again.
	wA := doReq(t, h, http.MethodPost, "A")
	if got := atomic.LoadInt32(&count); got != 4 {
		t.Fatalf("A was evicted but cache reported hit; invocations=%d", got)
	}
	if wA.Body.String() != "A" {
		t.Fatalf("A reply body: got %q", wA.Body.String())
	}
	if wA.Header().Get(IdempotencyCachedHeader) != "" {
		t.Fatalf("A was a fresh miss — must not be marked cached")
	}
}

func TestIdempotency_InFlightDedupSingleflight(t *testing.T) {
	var count int32
	gate := make(chan struct{})

	// Block the handler until the gate is closed. While blocked, fire
	// N concurrent requests with the same key and verify only one
	// makes it past the gate.
	h := IdempotencyKey(IdempotencyConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&count, 1)
		<-gate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	}))

	const N = 8
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = doReq(t, h, http.MethodPost, "same")
		}(i)
	}

	// Give the goroutines a moment to land in the singleflight queue,
	// then release the handler. (A short sleep is the canonical way
	// to assert "concurrent" here; we verify count==1 after, so a too-
	// short sleep would just produce a stricter check.)
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("singleflight should dedupe: handler ran %d times, want 1", got)
	}
	for i, w := range recs {
		if w.Code != http.StatusOK || w.Body.String() != "done" {
			t.Fatalf("req %d: status=%d body=%q", i, w.Code, w.Body.String())
		}
	}
}

func TestIdempotency_CachedHeaderEmitted(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{})(countingHandler(&count, http.StatusOK, `payload`))

	w1 := doReq(t, h, http.MethodPost, "k1")
	if got := w1.Header().Get(IdempotencyCachedHeader); got != "" {
		t.Fatalf("first call must not be marked cached, got %q", got)
	}

	w2 := doReq(t, h, http.MethodPost, "k1")
	if got := w2.Header().Get(IdempotencyCachedHeader); got != "true" {
		t.Fatalf("replay must set Idempotency-Cached: true, got %q", got)
	}
}

// Sanity test exercising the LRU touch-on-read behavior: a stale entry
// should be refreshed by a read so it doesn't get evicted while still
// in use.
func TestIdempotency_LRUTouchOnRead(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{MaxEntries: 2})(countingHandler(&count, http.StatusOK, `x`))

	doReq(t, h, http.MethodPost, "A")
	doReq(t, h, http.MethodPost, "B")
	// Touch A so it becomes most-recent.
	doReq(t, h, http.MethodPost, "A")
	// Insert C — should evict B (now oldest), NOT A.
	doReq(t, h, http.MethodPost, "C")

	priorCount := atomic.LoadInt32(&count)
	// A still cached → no new invocation.
	doReq(t, h, http.MethodPost, "A")
	if got := atomic.LoadInt32(&count); got != priorCount {
		t.Fatalf("A should have been kept hot by the touch; ran handler again (%d -> %d)", priorCount, got)
	}
}

// Ensure default RequiredMethods covers the common mutating verbs.
func TestIdempotency_DefaultMethodsCovered(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			var count int32
			h := IdempotencyKey(IdempotencyConfig{})(countingHandler(&count, http.StatusOK, fmt.Sprintf("ok-%s", m)))
			doReq(t, h, m, "k")
			doReq(t, h, m, "k")
			if got := atomic.LoadInt32(&count); got != 1 {
				t.Fatalf("%s: handler should be cached, ran %d times", m, got)
			}
		})
	}
}

// Smoke test: a configured custom set of methods is honored (and POST
// becomes a passthrough when not listed).
func TestIdempotency_CustomRequiredMethods(t *testing.T) {
	var count int32
	h := IdempotencyKey(IdempotencyConfig{
		RequiredMethods: []string{http.MethodPatch},
	})(countingHandler(&count, http.StatusOK, `ok`))

	doReq(t, h, http.MethodPost, "x")
	doReq(t, h, http.MethodPost, "x")
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("POST not in RequiredMethods → no caching; want 2 calls, got %d", got)
	}
	doReq(t, h, http.MethodPatch, "x")
	doReq(t, h, http.MethodPatch, "x")
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("PATCH cached: want +1 invocation (total 3), got %d", got)
	}
}

// Doc sanity: cached response includes the Content-Type the handler
// set, even when the handler explicitly used a non-default type.
func TestIdempotency_ContentTypePreserved(t *testing.T) {
	h := IdempotencyKey(IdempotencyConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a,b,c"))
	}))
	doReq(t, h, http.MethodPost, "k")
	w := doReq(t, h, http.MethodPost, "k")
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Fatalf("content-type preserved on replay; got %q", got)
	}
}

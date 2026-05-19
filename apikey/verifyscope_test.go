package apikey

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scopedVerifier returns a stub VerifyResult or error. Separate from
// fakeVerifier (in apikey_test.go) because VerifyScope needs to surface
// the actual scope string and we want call-count visibility.
type scopedVerifier struct {
	calls   int64
	respond func() (*VerifyResult, error)
}

func (s *scopedVerifier) Verify(_ context.Context, _ string) (*VerifyResult, error) {
	atomic.AddInt64(&s.calls, 1)
	return s.respond()
}

func newTestClient() *Client {
	// Production Client.New() reads env vars; we don't need a real
	// keystore for verifyScopeWith — it takes an explicit Verifier.
	return &Client{}
}

func freshCache() *scopeCache {
	return newScopeCache(scopeCacheMaxEntries, scopeCacheTTL)
}

func TestVerifyScope_MatchReturnsNil(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	if err := c.verifyScopeWith(context.Background(), freshCache(), v, "k", "read"); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestVerifyScope_MismatchReturnsErrScopeMismatch(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	err := c.verifyScopeWith(context.Background(), freshCache(), v, "k", "admin")
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("want ErrScopeMismatch, got %v", err)
	}
}

func TestVerifyScope_KeystoreErrorPropagates(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return nil, ErrKeystoreUnavailable
	}}
	c := newTestClient()
	err := c.verifyScopeWith(context.Background(), freshCache(), v, "k", "read")
	if !errors.Is(err, ErrKeystoreUnavailable) {
		t.Fatalf("want ErrKeystoreUnavailable, got %v", err)
	}

	v2 := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return nil, ErrInvalidKey
	}}
	err = c.verifyScopeWith(context.Background(), freshCache(), v2, "k", "read")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
}

func TestVerifyScope_CachedPositiveSkipsKeystore(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	sc := freshCache()
	ctx := context.Background()

	if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt64(&v.calls); got != 1 {
		t.Fatalf("first call should hit keystore once, got %d", got)
	}
	// Next N calls within TTL should not hit the keystore again.
	for i := 0; i < 20; i++ {
		if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
			t.Fatalf("cached call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&v.calls); got != 1 {
		t.Errorf("cached calls should stay at 1 upstream hit, got %d", got)
	}
}

func TestVerifyScope_NegativeNeverCached(t *testing.T) {
	// Keystore always reports a different scope than what the caller
	// claims — a second call must re-query (never cached).
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	sc := freshCache()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		err := c.verifyScopeWith(ctx, sc, v, "k", "admin")
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("iter %d: want ErrScopeMismatch, got %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&v.calls); got != 5 {
		t.Errorf("each mismatch should hit keystore; want 5 calls, got %d", got)
	}
	if got := sc.len(); got != 0 {
		t.Errorf("negative results must not populate cache; got %d entries", got)
	}

	// Also check error-path (ErrKeystoreUnavailable) is not cached.
	v2 := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return nil, ErrKeystoreUnavailable
	}}
	sc2 := freshCache()
	for i := 0; i < 3; i++ {
		_ = c.verifyScopeWith(ctx, sc2, v2, "k", "read")
	}
	if got := atomic.LoadInt64(&v2.calls); got != 3 {
		t.Errorf("upstream errors must not be cached; want 3 calls, got %d", got)
	}
}

func TestVerifyScope_CacheTTLExpiry(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	sc := newScopeCache(100, 100*time.Millisecond) // short TTL for the test
	now := time.Unix(1_000_000, 0)
	sc.setNow(func() time.Time { return now })
	ctx := context.Background()

	if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := atomic.LoadInt64(&v.calls); got != 1 {
		t.Fatalf("seed call count: got %d want 1", got)
	}

	// Within TTL — cache hit.
	now = now.Add(50 * time.Millisecond)
	if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
		t.Fatalf("within-TTL: %v", err)
	}
	if got := atomic.LoadInt64(&v.calls); got != 1 {
		t.Fatalf("within-TTL should not re-query, got %d", got)
	}

	// Past TTL — cache miss, re-query.
	now = now.Add(200 * time.Millisecond)
	if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
		t.Fatalf("post-TTL: %v", err)
	}
	if got := atomic.LoadInt64(&v.calls); got != 2 {
		t.Errorf("expired cache should re-query keystore; want 2 calls, got %d", got)
	}
}

func TestVerifyScope_CacheLRU_BoundedSize(t *testing.T) {
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "u", Scope: "s"}, nil
	}}
	c := newTestClient()
	max := 4
	sc := newScopeCache(max, time.Hour)
	ctx := context.Background()

	// Fill the cache with `max` distinct entries.
	for i := 0; i < max; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := c.verifyScopeWith(ctx, sc, v, key, "s"); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if got := sc.len(); got != max {
		t.Fatalf("cache should be full at %d, got %d", max, got)
	}

	// Add one more — oldest (k0) should be evicted.
	if err := c.verifyScopeWith(ctx, sc, v, "k_new", "s"); err != nil {
		t.Fatalf("overflow add: %v", err)
	}
	if got := sc.len(); got != max {
		t.Fatalf("cache should still be bounded at %d, got %d", max, got)
	}
	// k0 must be gone: looking up k0 should now miss.
	if sc.lookup(scopeCacheKey{key: "k0", scope: "s"}) {
		t.Errorf("oldest entry k0 should have been LRU-evicted")
	}
	// k1..k_new should still be hits.
	for _, k := range []string{"k1", "k2", "k3", "k_new"} {
		if !sc.lookup(scopeCacheKey{key: k, scope: "s"}) {
			t.Errorf("expected cache hit for %q after eviction; missed", k)
		}
	}
}

func TestVerifyScope_ConcurrentCallsThreadSafe(t *testing.T) {
	// N goroutines all calling the same (key,scope). They may race on
	// the first-fill; correctness only requires no panic, no data
	// race (run with -race), and that the final cache state is sane.
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: "read"}, nil
	}}
	c := newTestClient()
	sc := freshCache()
	ctx := context.Background()

	var wg sync.WaitGroup
	const N = 50
	const iters = 20
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Mix of (key,scope) pairs to exercise different code
				// paths concurrently.
				key := fmt.Sprintf("k%d", g%3)
				scope := "read"
				if i%5 == 0 {
					// Some mismatches — must not poison cache.
					scope = "admin"
					err := c.verifyScopeWith(ctx, sc, v, key, scope)
					if !errors.Is(err, ErrScopeMismatch) {
						t.Errorf("g=%d i=%d: want ErrScopeMismatch, got %v", g, i, err)
					}
					continue
				}
				if err := c.verifyScopeWith(ctx, sc, v, key, scope); err != nil {
					t.Errorf("g=%d i=%d: %v", g, i, err)
				}
			}
		}(g)
	}
	wg.Wait()

	// At most 3 distinct positive (key,scope) pairs should be cached.
	if got := sc.len(); got > 3 {
		t.Errorf("expected at most 3 positive cache entries, got %d", got)
	}
}

func TestVerifyScope_DistinctScopesAreSeparateCacheEntries(t *testing.T) {
	// Cache key is (key, scope) — a positive for scope=read must NOT
	// satisfy a verify with scope=write under the same key. Without
	// this, an attacker who got scope=read cached could request a
	// privileged scope and be served from cache.
	var currentScope atomic.Value
	currentScope.Store("read")
	v := &scopedVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "alice", Scope: currentScope.Load().(string)}, nil
	}}
	c := newTestClient()
	sc := freshCache()
	ctx := context.Background()

	// Seed cache with (k, read).
	if err := c.verifyScopeWith(ctx, sc, v, "k", "read"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	calls := atomic.LoadInt64(&v.calls)

	// Now ask for (k, write). Keystore still says scope=read, so
	// expect ErrScopeMismatch — AND the keystore must have been
	// re-queried (different cache key).
	currentScope.Store("read")
	err := c.verifyScopeWith(ctx, sc, v, "k", "write")
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("want ErrScopeMismatch on different-scope lookup, got %v", err)
	}
	if atomic.LoadInt64(&v.calls) == calls {
		t.Error("different (key,scope) MUST hit keystore — cache key collision")
	}
}

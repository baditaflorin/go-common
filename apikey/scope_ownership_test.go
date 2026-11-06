package apikey

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"weak"
)

func makeWeakScopeClient() weak.Pointer[Client] {
	c := &Client{}
	c.getScopeCache().store(scopeCacheKey{key: "test", scope: "read"})
	return weak.Make(c)
}

func TestCopiedInitializedClientDoesNotReuseAnotherKeystoresScopes(t *testing.T) {
	var allowedCalls, rejectedCalls atomic.Int32
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		allowedCalls.Add(1)
		w.Header().Set("X-Auth-Scope", "read")
		w.WriteHeader(http.StatusOK)
	}))
	defer allowed.Close()
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rejectedCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejected.Close()
	original := &Client{BaseURL: allowed.URL, HTTPClient: allowed.Client()}
	if err := original.VerifyScope(context.Background(), "fixture-key", "read"); err != nil {
		t.Fatal(err)
	}
	originalCache := original.getScopeCache()
	copied := *original
	copied.BaseURL, copied.HTTPClient = rejected.URL, rejected.Client()
	if err := copied.VerifyScope(context.Background(), "fixture-key", "read"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("copied client accepted another keystore's cached scope: %v", err)
	}
	if rejectedCalls.Load() != 1 || copied.getScopeCache() == originalCache {
		t.Fatal("copied client did not independently verify its scope")
	}
	if err := original.VerifyScope(context.Background(), "fixture-key", "read"); err != nil || allowedCalls.Load() != 1 {
		t.Fatalf("copy disturbed the original cache: error=%v calls=%d", err, allowedCalls.Load())
	}
}

func TestScopeCacheDoesNotRetainDiscardedClient(t *testing.T) {
	client := makeWeakScopeClient()
	runtime.GC()
	if client.Value() != nil {
		t.Fatal("scope cache retains discarded client")
	}
}

func TestScopeCacheConcurrentInitializationHasOneOwner(t *testing.T) {
	c := &Client{}
	var wg sync.WaitGroup
	caches := make(chan *scopeCache, 64)
	for range 64 {
		wg.Add(1)
		go func() { defer wg.Done(); caches <- c.getScopeCache() }()
	}
	wg.Wait()
	close(caches)
	expected := c.getScopeCache()
	for got := range caches {
		if got != expected {
			t.Fatal("concurrent initialization produced separate caches")
		}
	}
	if other := (&Client{}).getScopeCache(); other == expected {
		t.Fatal("separate clients unexpectedly share cache")
	}
}

func BenchmarkScopeCacheAccess(b *testing.B) {
	c := &Client{}
	c.getScopeCache()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = c.getScopeCache()
	}
}

package proxysupplier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mockWebshareListServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	entries := make([]webshareEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = webshareEntry{
			Username: "u", Password: "p",
			ProxyAddress: "10.0.0.1", Port: 5000 + i, Valid: true,
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-key" {
			t.Errorf("unexpected Authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{
			Count: n, Next: "", Results: entries,
		})
	}))
}

// withWebshareDirectBaseURL points the package-level base URL at a test
// server for the duration of the test, restoring it afterward.
func withWebshareDirectBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := webshareDirectDefaultBaseURL
	webshareDirectDefaultBaseURL = url + "/"
	t.Cleanup(func() { webshareDirectDefaultBaseURL = orig })
}

// waitForNonEmptyProxyURL polls ProxyURL() until it returns something or
// the deadline passes. The initial fetch is deliberately asynchronous
// (see newWebshareDirectSupplier's doc comment) so tests must wait for it
// instead of asserting immediately after construction.
func waitForNonEmptyProxyURL(t *testing.T, s Supplier, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if u := s.ProxyURL(); u != "" {
			return u
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("ProxyURL() still empty after %s", timeout)
	return ""
}

func TestWebshareDirect_NoAPIKeyReturnsNone(t *testing.T) {
	s := NewFromConfig(Config{Supplier: "webshare_direct"})
	if s.Name() != "none" {
		t.Fatalf("want none supplier when WebshareAPIKey is empty, got %q", s.Name())
	}
}

func TestWebshareDirect_FetchesAndRoundRobins(t *testing.T) {
	srv := mockWebshareListServer(t, 5)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	if s.Name() != "webshare_direct" {
		t.Fatalf("want webshare_direct supplier, got %q", s.Name())
	}

	// The background fetch (see newWebshareDirectSupplier) needs a moment
	// to complete; wait for the first non-empty result before asserting
	// on distribution.
	waitForNonEmptyProxyURL(t, s, time.Second)

	// Every entry should appear, and appear an equal number of times, over
	// several full cycles -- this is the "rotate evenly" requirement.
	const rounds = 4
	seen := map[string]int{}
	for i := 0; i < 5*rounds; i++ {
		u := s.ProxyURL()
		if u == "" {
			t.Fatal("ProxyURL() returned empty for a populated pool")
		}
		seen[u]++
	}
	if len(seen) != 5 {
		t.Fatalf("expected all 5 distinct entries visited, got %d distinct", len(seen))
	}
	for u, count := range seen {
		if count != rounds {
			t.Errorf("entry %s visited %d times, want exactly %d (perfect round-robin)", u, count, rounds)
		}
	}
}

func TestWebshareDirect_ProxyURLEmptyBeforeFirstFetchCompletes(t *testing.T) {
	// A server that stalls past the test's own patience simulates the
	// exact condition that motivated making the initial fetch
	// asynchronous: a slow/degraded Webshare API must not block anything
	// that calls ProxyURL() early -- it just gets "" (no proxy), the same
	// safe fallback every other misconfigured supplier already produces.
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	if s.Name() != "webshare_direct" {
		t.Fatalf("want webshare_direct supplier even while the fetch is still in flight, got %q", s.Name())
	}
	if u := s.ProxyURL(); u != "" {
		t.Fatalf("ProxyURL() = %q, want \"\" while the initial fetch is still pending", u)
	}

	// HTTPClient must remain usable during the asynchronous initial refresh.
	// Its transport will use a direct connection until the pool is populated,
	// then resolve a proxy URL on each subsequent request.
	client := HTTPClient(s, time.Second)
	if client == nil {
		t.Fatal("HTTPClient() = nil while webshare_direct refresh is pending")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("HTTPClient transport must expose a proxy resolver")
	}
	req := httptest.NewRequest(http.MethodGet, "https://duckduckgo.com/html/", nil)
	proxyURL, err := tr.Proxy(req)
	if err != nil || proxyURL != nil {
		t.Fatalf("pending pool proxy resolver = %v, %v; want direct nil proxy", proxyURL, err)
	}
}

func TestWebshareDirect_HTTPClientKeepsFreshConnectionPerRequest(t *testing.T) {
	srv := mockWebshareListServer(t, 3)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second) // HTTPClient returns nil for an empty ProxyURL()
	client := HTTPClient(s, 0)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	// webshareDirectSupplier does not implement keepAliver, so HTTPClient
	// must fall back to its default: a fresh connection (and therefore a
	// freshly round-robined IP) on every request. This is the entire point
	// of this supplier for an IP-reputation-sensitive target -- silently
	// reusing a connection here would defeat the rotation ProxyURL()
	// already guarantees.
	if !tr.DisableKeepAlives {
		t.Error("DisableKeepAlives = false, want true (webshare_direct must not reuse connections, or its round-robin IP diversity is defeated)")
	}
}

func TestWebshareDirect_AllInvalidEntriesLeavesProxyURLEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: 1, Results: []webshareEntry{
			{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5000, Valid: false},
		}})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	// The supplier type is fixed at construction (webshare_direct, not a
	// fallback to none) since whether the fetch will succeed isn't known
	// synchronously anymore -- what's safe is that ProxyURL() keeps
	// returning "" (no proxy) rather than ever returning a URL built from
	// invalid/empty data. Give the background fetch time to run and fail.
	time.Sleep(50 * time.Millisecond)
	if u := s.ProxyURL(); u != "" {
		t.Fatalf("ProxyURL() = %q, want \"\" when every fetched entry is invalid", u)
	}
}

func TestWebshareDirect_MarkResultSkipsFailedEntryUntilCooldownElapses(t *testing.T) {
	srv := mockWebshareListServer(t, 3)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	rr, ok := s.(ResultReporter)
	if !ok {
		t.Fatal("webshare_direct supplier must implement ResultReporter")
	}
	first := waitForNonEmptyProxyURL(t, s, time.Second)
	addr := AddrFromProxyURL(first)

	// One bad result is enough to trip the cooldown (webshareDirectMaxConsecutiveFailures = 1).
	rr.MarkResult(addr, false)

	for i := 0; i < 10; i++ {
		if u := s.ProxyURL(); u == first {
			t.Fatalf("Next() returned the just-failed entry %s after MarkResult(ok=false), want it skipped", first)
		}
	}
}

func TestWebshareDirect_MarkResultOKClearsCooldownImmediately(t *testing.T) {
	srv := mockWebshareListServer(t, 2)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	rr := s.(ResultReporter)
	first := waitForNonEmptyProxyURL(t, s, time.Second)
	addr := AddrFromProxyURL(first)

	rr.MarkResult(addr, false)
	rr.MarkResult(addr, true) // a later success should clear the cooldown right away

	seenAgain := false
	for i := 0; i < 10; i++ {
		if u := s.ProxyURL(); u == first {
			seenAgain = true
			break
		}
	}
	if !seenAgain {
		t.Fatal("entry should be eligible again immediately after MarkResult(ok=true)")
	}
}

func TestWebshareDirect_ProxyURLEmptyWhenEveryEntryInCooldown(t *testing.T) {
	srv := mockWebshareListServer(t, 2)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	rr := s.(ResultReporter)
	waitForNonEmptyProxyURL(t, s, time.Second)

	// Fail every entry in the pool.
	seen := map[string]bool{}
	for len(seen) < 2 {
		u := s.ProxyURL()
		if u == "" {
			t.Fatal("ProxyURL() went empty before every entry was marked failed")
		}
		addr := AddrFromProxyURL(u)
		if !seen[addr] {
			rr.MarkResult(addr, false)
			seen[addr] = true
		}
	}

	if u := s.ProxyURL(); u != "" {
		t.Fatalf("ProxyURL() = %q, want \"\" when every entry is in cooldown (fail closed against a rate limiter, not open)", u)
	}
}

func TestWebshareDirect_RefreshPreservesFailureStateForSurvivingEntries(t *testing.T) {
	entries := []webshareEntry{
		{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5001, Valid: true},
		{Username: "u", Password: "p", ProxyAddress: "10.0.0.2", Port: 5002, Valid: true},
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(entries), Results: entries})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	rr := s.(ResultReporter)
	wds := s.(*webshareDirectSupplier)
	first := waitForNonEmptyProxyURL(t, s, time.Second)
	addr := AddrFromProxyURL(first)
	rr.MarkResult(addr, false)

	// A manual refresh with the same account list (as a real 10-minute
	// tick would fetch) must not reset that entry's cooldown -- same
	// rationale as webshareproxy.Pool's own refresh test this session.
	if err := wds.refresh(context.Background()); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}

	for i := 0; i < 10; i++ {
		if u := s.ProxyURL(); u == first {
			t.Fatalf("Next() returned %s after a refresh, want it to stay in cooldown across refreshes", first)
		}
	}
}

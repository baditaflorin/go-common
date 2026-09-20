package proxysupplier

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestWebshareDirect_ProxyURLForKeyIsStableAcrossCalls is the core
// guarantee: the same key must return the same proxy URL on repeated
// calls, unlike ProxyURL()'s per-call round-robin.
func TestWebshareDirect_ProxyURLForKeyIsStableAcrossCalls(t *testing.T) {
	srv := mockWebshareListServer(t, 20)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second)

	ks, ok := s.(KeyedSupplier)
	if !ok {
		t.Fatal("webshare_direct supplier does not implement KeyedSupplier")
	}

	first := ks.ProxyURLForKey("session-42")
	if first == "" {
		t.Fatal("ProxyURLForKey returned empty for a populated pool")
	}
	for i := 0; i < 10; i++ {
		if got := ks.ProxyURLForKey("session-42"); got != first {
			t.Fatalf("call %d: ProxyURLForKey(%q) = %q, want stable %q", i, "session-42", got, first)
		}
	}
}

// TestWebshareDirect_ProxyURLForKeyDoesNotAdvanceRoundRobinCursor proves
// keyed and unkeyed callers don't interfere: calling ProxyURLForKey
// repeatedly must not perturb ProxyURL()'s own round-robin sequence for a
// concurrent unkeyed caller.
func TestWebshareDirect_ProxyURLForKeyDoesNotAdvanceRoundRobinCursor(t *testing.T) {
	srv := mockWebshareListServer(t, 5)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second)

	// Capture the round-robin sequence with no keyed calls interleaved.
	baseline := make([]string, 10)
	for i := range baseline {
		baseline[i] = s.ProxyURL()
	}

	// Reset by re-fetching a fresh supplier so the cursor starts over,
	// then interleave a burst of keyed calls and confirm the unkeyed
	// sequence is unchanged.
	s2 := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s2, time.Second)
	ks2 := s2.(KeyedSupplier)

	interleaved := make([]string, 10)
	for i := range interleaved {
		for j := 0; j < 5; j++ {
			ks2.ProxyURLForKey("noise-key")
		}
		interleaved[i] = s2.ProxyURL()
	}

	for i := range baseline {
		if baseline[i] != interleaved[i] {
			t.Fatalf("round-robin sequence diverged at step %d after interleaved ProxyURLForKey calls: %q vs %q", i, baseline[i], interleaved[i])
		}
	}
}

// TestWebshareDirect_ProxyURLForKeyRespectsCooldown covers the fallback
// path: a key whose preferred entry is in cooldown must fall through to
// the next eligible entry, same as ProxyURL().
func TestWebshareDirect_ProxyURLForKeyRespectsCooldown(t *testing.T) {
	srv := mockWebshareListServer(t, 3)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second)
	ks := s.(KeyedSupplier)
	rr := s.(ResultReporter)

	key := "sticky-target"
	pinned := ks.ProxyURLForKey(key)
	if pinned == "" {
		t.Fatal("expected a non-empty pinned proxy")
	}

	// Drive the pinned entry into cooldown (webshareDirectMaxConsecutiveFailures == 1).
	addr := AddrFromProxyURL(pinned)
	rr.MarkResult(addr, false)

	fallback := ks.ProxyURLForKey(key)
	if fallback == "" {
		t.Fatal("expected a fallback proxy once the preferred entry is cooling down")
	}
	if fallback == pinned {
		t.Fatalf("ProxyURLForKey returned the cooling-down entry %q instead of falling forward", pinned)
	}

	// Recovery: a success clears the cooldown and the key should return to
	// its original preferred entry.
	rr.MarkResult(addr, true)
	if recovered := ks.ProxyURLForKey(key); recovered != pinned {
		t.Fatalf("after MarkResult(ok=true), ProxyURLForKey(%q) = %q, want recovered %q", key, recovered, pinned)
	}
}

// TestWebshareDirect_ProxyURLForKeyDistributesAcrossPool is a light sanity
// check that different keys don't all degenerate onto the same entry.
func TestWebshareDirect_ProxyURLForKeyDistributesAcrossPool(t *testing.T) {
	srv := mockWebshareListServer(t, 50)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second)
	ks := s.(KeyedSupplier)

	seen := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		key := "target-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		seen[ks.ProxyURLForKey(key)] = struct{}{}
	}
	if len(seen) < 5 {
		t.Fatalf("50 distinct keys mapped to only %d distinct proxies — hash distribution looks degenerate", len(seen))
	}
}

// TestHTTPClient_StickyKeyHeaderSelectsPinnedProxy is the integration
// point: HTTPClient must consult StickyKeyHeader, use ProxyURLForKey when
// present, and strip the header before the request is sent.
func TestHTTPClient_StickyKeyHeaderSelectsPinnedProxy(t *testing.T) {
	srv := mockWebshareListServer(t, 10)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	waitForNonEmptyProxyURL(t, s, time.Second)
	ks := s.(KeyedSupplier)

	want := ks.ProxyURLForKey("pinned-session")

	client := HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)

	req := httptest.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	req.Header.Set(StickyKeyHeader, "pinned-session")

	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(): %v", err)
	}
	if got == nil {
		t.Fatal("Proxy() returned nil, want the pinned proxy URL")
	}
	if gotAddr, wantAddr := got.Host, mustParseHost(t, want); gotAddr != wantAddr {
		t.Errorf("Proxy() host = %q, want %q (from ProxyURLForKey)", gotAddr, wantAddr)
	}
	if req.Header.Get(StickyKeyHeader) != "" {
		t.Error("StickyKeyHeader was not stripped from the outbound request — would leak to the proxy/origin")
	}
}

// TestHTTPClient_StickyKeyHeaderIgnoredForNonKeyedSupplier confirms
// backward compatibility: a supplier that doesn't implement KeyedSupplier
// (e.g. plain_proxies) must still work normally, and the header must still
// be stripped so it never leaks even though nothing consumes it.
func TestHTTPClient_StickyKeyHeaderIgnoredForNonKeyedSupplier(t *testing.T) {
	s := NewFromConfig(Config{Supplier: "plain_proxies", Host: "proxy.example", Port: "8080", Username: "u", Password: "p"})
	client := HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)

	req := httptest.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	req.Header.Set(StickyKeyHeader, "whatever")

	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy(): %v", err)
	}
	if got == nil {
		t.Fatal("Proxy() returned nil, want the plain_proxies gateway URL")
	}
	if req.Header.Get(StickyKeyHeader) != "" {
		t.Error("StickyKeyHeader was not stripped even though the supplier isn't a KeyedSupplier")
	}
}

func mustParseHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}

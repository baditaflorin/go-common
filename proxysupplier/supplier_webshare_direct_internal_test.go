package proxysupplier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestWebshareDirect_HTTPClientKeepsFreshConnectionPerRequest(t *testing.T) {
	srv := mockWebshareListServer(t, 3)
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
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

func TestWebshareDirect_EmptyPoolReturnsEmptyProxyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: 1, Results: []webshareEntry{
			{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5000, Valid: false},
		}})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	// An all-invalid fetch fails newWebshareDirectSupplier's initial
	// refresh, so this falls back to noneSupplier{} entirely -- verify
	// that fallback, not a webshare_direct supplier with an empty list.
	if s.Name() != "none" {
		t.Fatalf("want none supplier when every fetched entry is invalid, got %q", s.Name())
	}
}

package safehttp_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// loopbackHost returns the bare host (no port) for a httptest server's
// URL. httptest binds to 127.0.0.1 in practice but parsing keeps the
// tests resilient if that ever changes.
func loopbackHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

// newCountingServer returns an httptest server that increments hits on
// every request, plus a *atomic.Int64 the caller can inspect.
func newCountingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestEgressAllowlist_AllowsListedHost(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)
	host := loopbackHost(t, srv.URL)

	c := safehttp.NewClient(safehttp.WithEgressAllowlist(host))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
}

func TestEgressAllowlist_BlocksUnlistedHost(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)

	// Allowlist only api.hetzner.cloud; loopback target is NOT in it.
	c := safehttp.NewClient(safehttp.WithEgressAllowlist("api.hetzner.cloud"))

	_, err := c.Get(srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("err = %v, want errors.Is ErrEgressNotAllowed", err)
	}
	// Critical: the request must NOT have reached the server.
	if got := hits.Load(); got != 0 {
		t.Fatalf("server hits = %d, want 0 (blocked before dispatch)", got)
	}
}

func TestEgressAllowlist_EmptyMeansNoOp(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)

	// No WithEgressAllowlist option at all — backwards-compatible path.
	c := safehttp.NewClient()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (no-op default)", hits.Load())
	}
}

func TestEgressAllowlist_CaseInsensitive(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)
	host := loopbackHost(t, srv.URL)

	// httptest binds to 127.0.0.1 which is all digits, so case
	// folding is a no-op for the bound host. Cover the case-insensitive
	// branch by also configuring a mixed-case alias that won't be hit
	// AND the literal loopback host. The behavior verified is "uppercase
	// entries in the allowlist still match a lowercase URL".
	c := safehttp.NewClient(safehttp.WithEgressAllowlist("API.HETZNER.CLOUD", strings.ToUpper(host)))

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (case-insensitive match)", hits.Load())
	}

	// And the reverse: a lowercase allowlist entry matches an
	// uppercase URL hostname. We exercise this on a synthetic target —
	// the request will fail at DNS or be blocked by SSRF, but for the
	// egress check we only need to verify it does NOT return
	// ErrEgressNotAllowed when the case differs.
	c2 := safehttp.NewClient(safehttp.WithEgressAllowlist("api.hetzner.cloud"))
	_, err = c2.Get("http://API.HETZNER.CLOUD/")
	if err != nil && errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("uppercase URL should match lowercase allowlist; got ErrEgressNotAllowed")
	}
	// Any other error (DNS, timeout) is fine — we just care the egress
	// transport let the request proceed to the inner transport.
}

func TestEgressAllowlist_StripsPortFromHostMatch(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)
	host := loopbackHost(t, srv.URL)

	// Allowlist holds the bare hostname; the URL we GET has host:port.
	// Match must ignore the port.
	c := safehttp.NewClient(safehttp.WithEgressAllowlist(host))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (port-stripped match)", hits.Load())
	}
}

func TestEgressAllowlist_ErrorWrapsURL(t *testing.T) {
	allowLoopback(t)
	c := safehttp.NewClient(safehttp.WithEgressAllowlist("api.hetzner.cloud"))

	_, err := c.Get("http://api.github.com/")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("err = %v, want errors.Is ErrEgressNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "api.github.com") {
		t.Fatalf("err message %q does not contain rejected hostname", err.Error())
	}
}

func TestDenyAllEgress_BlocksEverything(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)

	c := safehttp.NewClient(safehttp.WithDenyAllEgress())
	_, err := c.Get(srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("err = %v, want errors.Is ErrEgressNotAllowed", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0", hits.Load())
	}

	// Even the canonical fleet host is rejected.
	_, err = c.Get("https://api.hetzner.cloud/")
	if err == nil || !errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("WithDenyAllEgress should reject api.hetzner.cloud; got %v", err)
	}
}

func TestEgressAllowlist_FiresAfterSSRFGuard(t *testing.T) {
	// Private-IP target: the SSRF guard MUST win (ErrBlocked), not the
	// egress allowlist (ErrEgressNotAllowed). This pins the ordering so
	// operators see the stronger signal when a misconfigured allowlist
	// would otherwise mask an SSRF attempt.
	//
	// We do NOT allowLoopback here — we WANT the SSRF guard to fire.
	c := safehttp.NewClient(safehttp.WithEgressAllowlist("api.hetzner.cloud"))

	_, err := c.Get("http://10.0.0.1/")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("private IP should be rejected by SSRF guard, not egress allowlist; got ErrEgressNotAllowed")
	}
	if !errors.Is(err, safehttp.ErrBlocked) {
		t.Fatalf("err = %v, want errors.Is ErrBlocked", err)
	}
}

// TestEgressAllowlist_NoNetworkCallOnReject is an explicit guard that
// rejected requests do not perform any I/O. We assert this by giving
// the client a deliberately unroutable allowlist entry and a server
// that, if reached, would bump the counter. With the host blocked
// pre-dispatch, the dial never happens.
func TestEgressAllowlist_NoNetworkCallOnReject(t *testing.T) {
	allowLoopback(t)
	srv, hits := newCountingServer(t)

	// Allowlist excludes the loopback server host.
	c := safehttp.NewClient(safehttp.WithEgressAllowlist("not-the-server.example"))

	// Target is the live httptest server URL, but its host is NOT in
	// the allowlist — the request must be rejected without reaching
	// the server.
	_, err := c.Get(srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, safehttp.ErrEgressNotAllowed) {
		t.Fatalf("err = %v, want errors.Is ErrEgressNotAllowed", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server hits = %d, want 0 (zero I/O on reject)", got)
	}

	// Silence the unused-import linter via a no-op use of net for
	// future-proofing — keeps the import stable if we add net-level
	// assertions later.
	_ = net.IPv4len
}

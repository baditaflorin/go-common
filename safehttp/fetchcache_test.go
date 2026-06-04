package safehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubDelegate is a test FetchDelegate. If err is non-nil it returns
// that error (forcing the fall-through path); otherwise it returns a
// FetchResult built from status/body and records the URLs it saw.
type stubDelegate struct {
	status int
	body   string
	err    error

	mu       chanGuard
	seenURLs []string
}

// chanGuard is a tiny mutex stand-in to avoid importing sync just for
// the test recorder (keeps the stub trivially concurrency-safe).
type chanGuard struct{ ch chan struct{} }

func (g *chanGuard) lock() {
	if g.ch == nil {
		g.ch = make(chan struct{}, 1)
	}
	g.ch <- struct{}{}
}
func (g *chanGuard) unlock() { <-g.ch }

func (d *stubDelegate) FetchGet(_ context.Context, url string, _ http.Header) (*FetchResult, error) {
	d.mu.lock()
	d.seenURLs = append(d.seenURLs, url)
	d.mu.unlock()
	if d.err != nil {
		return nil, d.err
	}
	return &FetchResult{
		Status: d.status,
		Header: http.Header{"X-From-Delegate": []string{"1"}},
		Body:   []byte(d.body),
	}, nil
}

// allowLoopback lets the SSRF guard reach the httptest origin (127.0.0.1)
// for tests that assert the direct (non-delegate) path. Restored on cleanup.
func allowLoopback(t *testing.T) {
	t.Helper()
	SetAllowedPrivateIPs(parseAllowedPrivateIPs("127.0.0.1"))
	t.Cleanup(func() { SetAllowedPrivateIPs(nil) })
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// (a) With a stub delegate installed via SetDefaultFetchDelegate, a GET
// through NewClient() returns the delegate's body and never hits the
// network (the request targets an unroutable host to prove it).
func TestDefaultFetchDelegate_RoutesGet(t *testing.T) {
	d := &stubDelegate{status: 200, body: "from-delegate"}
	SetDefaultFetchDelegate(d)
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) })

	c := NewClient()
	// .invalid is reserved (RFC 6761) and never resolves, so if the
	// delegate were bypassed this would be a DNS/dial error, not a 200.
	req, err := http.NewRequest(http.MethodGet, "http://does-not-resolve.invalid/path?x=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := readBody(t, resp); got != "from-delegate" {
		t.Fatalf("body = %q, want %q", got, "from-delegate")
	}
	if resp.Header.Get("X-From-Delegate") != "1" {
		t.Fatalf("missing delegate header; got %v", resp.Header)
	}
	if len(d.seenURLs) != 1 || !strings.Contains(d.seenURLs[0], "does-not-resolve.invalid") {
		t.Fatalf("delegate did not see the request URL: %v", d.seenURLs)
	}
}

// (b) Delegate returning an error falls through to the real path.
func TestDefaultFetchDelegate_ErrorFallsThrough(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "from-origin")
	}))
	t.Cleanup(srv.Close)

	d := &stubDelegate{err: errors.New("cache down")}
	SetDefaultFetchDelegate(d)
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) })

	c := NewClient()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201 (origin)", resp.StatusCode)
	}
	if got := readBody(t, resp); got != "from-origin" {
		t.Fatalf("body = %q, want origin response", got)
	}
	if len(d.seenURLs) != 1 {
		t.Fatalf("delegate should have been consulted once, saw %v", d.seenURLs)
	}
}

// (c) A non-GET (POST) bypasses the delegate entirely.
func TestDefaultFetchDelegate_NonGetBypasses(t *testing.T) {
	allowLoopback(t)
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(202)
		_, _ = io.WriteString(w, "origin-post")
	}))
	t.Cleanup(srv.Close)

	d := &stubDelegate{status: 200, body: "from-delegate"}
	SetDefaultFetchDelegate(d)
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) })

	c := NewClient()
	resp, err := c.Post(srv.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.StatusCode != 202 || readBody(t, resp) != "origin-post" {
		t.Fatalf("POST did not reach origin; status=%d", resp.StatusCode)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("origin saw method %q", gotMethod)
	}
	if len(d.seenURLs) != 0 {
		t.Fatalf("delegate should NOT see a POST, saw %v", d.seenURLs)
	}
}

// (d) A WithoutProxy() client does NOT use the default delegate — it
// must keep real direct egress (hits the origin).
func TestDefaultFetchDelegate_WithoutProxyIgnoresDefault(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(203)
		_, _ = io.WriteString(w, "direct-origin")
	}))
	t.Cleanup(srv.Close)

	d := &stubDelegate{status: 200, body: "from-delegate"}
	SetDefaultFetchDelegate(d)
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) })

	c := NewClient(WithoutProxy())
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.StatusCode != 203 || readBody(t, resp) != "direct-origin" {
		t.Fatalf("WithoutProxy client should hit origin directly; status=%d", resp.StatusCode)
	}
	if len(d.seenURLs) != 0 {
		t.Fatalf("WithoutProxy client must NOT consult the default delegate, saw %v", d.seenURLs)
	}
}

// WithoutFetchCache opts a client out of the default delegate even
// without WithoutProxy.
func TestWithoutFetchCache_IgnoresDefault(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)

	d := &stubDelegate{status: 200, body: "from-delegate"}
	SetDefaultFetchDelegate(d)
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) })

	c := NewClient(WithoutFetchCache())
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204 (origin)", resp.StatusCode)
	}
	if len(d.seenURLs) != 0 {
		t.Fatalf("WithoutFetchCache client must not consult the delegate, saw %v", d.seenURLs)
	}
}

// A per-client WithFetchDelegate applies even when no default is set,
// and even for a WithoutProxy client (explicit opt-in wins).
func TestWithFetchDelegate_PerClient(t *testing.T) {
	t.Cleanup(func() { SetDefaultFetchDelegate(nil) }) // ensure clean default

	d := &stubDelegate{status: 200, body: "per-client"}
	c := NewClient(WithoutProxy(), WithFetchDelegate(d))
	req, _ := http.NewRequest(http.MethodGet, "http://does-not-resolve.invalid/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if readBody(t, resp) != "per-client" {
		t.Fatalf("explicit per-client delegate should win even with WithoutProxy")
	}
}

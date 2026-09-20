package safehttp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type captureObserver struct {
	mu     sync.Mutex
	events []EgressEvent
}

func (c *captureObserver) ObserveEgress(ev EgressEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *captureObserver) snapshot() []EgressEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EgressEvent, len(c.events))
	copy(out, c.events)
	return out
}

// TestObserverFiresOnSuccess: a 200 should produce one OutcomeSuccess
// event with correct host/scheme/method, ViaProxy=false (no proxy
// configured), and a non-zero duration.
func TestObserverFiresOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	obs := &captureObserver{}
	// 127.0.0.1 is blocked by IsBlocked by default — allow it for this test.
	u, _ := url.Parse(srv.URL)
	SetAllowedPrivateIPs(parseAllowedPrivateIPs("127.0.0.1"))
	defer SetAllowedPrivateIPs(nil)

	c := NewClient(WithObserver(obs), WithTimeout(2*time.Second))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	evs := obs.snapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
	if ev.Status != 200 {
		t.Errorf("status = %d, want 200", ev.Status)
	}
	if ev.Method != "GET" {
		t.Errorf("method = %q", ev.Method)
	}
	if ev.Host != u.Hostname() {
		t.Errorf("host = %q, want %q", ev.Host, u.Hostname())
	}
	if ev.Scheme != "http" {
		t.Errorf("scheme = %q", ev.Scheme)
	}
	if ev.ViaProxy {
		t.Errorf("via_proxy = true, want false (no proxy configured)")
	}
	if ev.Duration <= 0 {
		t.Errorf("duration not recorded")
	}
	if ev.Bytes != 5 {
		t.Errorf("bytes = %d, want 5 (from Content-Length)", ev.Bytes)
	}
	if ev.Err != nil {
		t.Errorf("err = %v, want nil", ev.Err)
	}
}

// TestObserverFiresOnSSRFBlock: dialing a guarded address with the SSRF
// guard active produces an OutcomeBlocked event with err == ErrBlocked
// (wrapped) and zero status.
func TestObserverFiresOnSSRFBlock(t *testing.T) {
	obs := &captureObserver{}
	c := NewClient(WithObserver(obs), WithTimeout(2*time.Second))
	// 10.0.0.1 is RFC1918 — IsBlocked = true.
	_, err := c.Get("http://10.0.0.1/")
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	evs := obs.snapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Outcome != OutcomeBlocked {
		t.Errorf("outcome = %q, want blocked (err=%v)", ev.Outcome, ev.Err)
	}
	if !errors.Is(ev.Err, ErrBlocked) {
		t.Errorf("err = %v, want wrapping ErrBlocked", ev.Err)
	}
	if ev.Status != 0 {
		t.Errorf("status = %d, want 0 on block", ev.Status)
	}
}

// TestResolveProxy covers the proxy-detection path used to populate
// ViaProxy / ProxyHost on EgressEvent. We test resolveProxy directly
// rather than through env vars because net/http caches
// ProxyFromEnvironment at first use process-wide, which makes env-based
// tests order-dependent.
func TestResolveProxy(t *testing.T) {
	mkReq := func() *http.Request {
		r, _ := http.NewRequest("GET", "http://example.com/", nil)
		return r
	}

	// nil proxyFn → direct.
	tNil := &extrasTransport{proxyFn: nil}
	if via, h := tNil.resolveProxy(mkReq()); via || h != "" {
		t.Errorf("nil proxyFn: via=%v host=%q, want false/\"\"", via, h)
	}

	// proxyFn returns nil URL → direct (the std-lib behaviour for
	// "no proxy configured for this request").
	tDirect := &extrasTransport{proxyFn: func(*http.Request) (*url.URL, error) {
		return nil, nil
	}}
	if via, h := tDirect.resolveProxy(mkReq()); via || h != "" {
		t.Errorf("nil URL: via=%v host=%q, want false/\"\"", via, h)
	}

	// proxyFn returns a real URL → ViaProxy=true.
	pu, _ := url.Parse("http://proxy.example:3128")
	tProxy := &extrasTransport{proxyFn: func(*http.Request) (*url.URL, error) {
		return pu, nil
	}}
	if via, h := tProxy.resolveProxy(mkReq()); !via || h != "proxy.example:3128" {
		t.Errorf("via_proxy: via=%v host=%q, want true/proxy.example:3128", via, h)
	}

	// proxyFn errors out → treat as direct (fail-open).
	tErr := &extrasTransport{proxyFn: func(*http.Request) (*url.URL, error) {
		return nil, errors.New("boom")
	}}
	if via, h := tErr.resolveProxy(mkReq()); via || h != "" {
		t.Errorf("err path: via=%v host=%q, want false/\"\"", via, h)
	}
}

// TestObserverBytesReflectsActualReadForChunkedResponse is the case
// responseBytes() alone got wrong: a response with no Content-Length header
// (net/http auto-chunks when a handler writes without setting it) used to
// report Bytes: 0 unconditionally. The event must now carry the real byte
// count, but only once the body is drained and closed.
func TestObserverBytesReflectsActualReadForChunkedResponse(t *testing.T) {
	const want = "this response deliberately omits Content-Length"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // forces chunked transfer-encoding, no Content-Length
		_, _ = w.Write([]byte(want))
	}))
	defer srv.Close()

	obs := &captureObserver{}
	SetAllowedPrivateIPs(parseAllowedPrivateIPs("127.0.0.1"))
	defer SetAllowedPrivateIPs(nil)

	c := NewClient(WithObserver(obs), WithTimeout(2*time.Second))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.ContentLength > 0 {
		t.Fatalf("test setup invalid: got Content-Length=%d, want unset (0 or -1) so this actually exercises the chunked path", resp.ContentLength)
	}

	// Event must not have fired yet — it's deferred to Close().
	if got := len(obs.snapshot()); got != 0 {
		t.Fatalf("events before Close() = %d, want 0 (emission should be deferred)", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	resp.Body.Close()

	evs := obs.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events after Close() = %d, want 1", len(evs))
	}
	if got := evs[0].Bytes; got != int64(len(want)) {
		t.Errorf("bytes = %d, want %d (actual bytes read, not a Content-Length guess)", got, len(want))
	}
	if evs[0].Channel != "direct" {
		t.Errorf("channel = %q, want %q", evs[0].Channel, "direct")
	}
}

// TestObserverBytesReflectsPartialReadOnEarlyClose documents the one known
// limitation of application-layer byte counting (see countingBody's doc
// comment): a caller that reads only part of the body before closing sees
// only what it actually consumed, not the full origin response size.
func TestObserverBytesReflectsPartialReadOnEarlyClose(t *testing.T) {
	full := strings.Repeat("a", 10_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	obs := &captureObserver{}
	SetAllowedPrivateIPs(parseAllowedPrivateIPs("127.0.0.1"))
	defer SetAllowedPrivateIPs(nil)

	c := NewClient(WithObserver(obs), WithTimeout(2*time.Second))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	const capBytes = 100
	partial := make([]byte, capBytes)
	if _, err := io.ReadFull(resp.Body, partial); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	resp.Body.Close()

	evs := obs.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].Bytes; got != capBytes {
		t.Errorf("bytes = %d, want %d (only what was actually read before closing)", got, capBytes)
	}
}

// TestObserverEmitsCacheHitChannel covers the fetch-cache delegate path,
// which previously never emitted an EgressEvent at all (it returns before
// reaching the RoundTrip code where the observer normally fires). Bytes must
// be exact — the delegate's response is already fully materialized, no live
// stream to under/over-count.
func TestObserverEmitsCacheHitChannel(t *testing.T) {
	obs := &captureObserver{}
	d := &stubDelegate{status: 200, body: "cached response body"}
	c := NewClient(WithFetchDelegate(d), WithObserver(obs), WithoutProxy())

	resp, err := c.Get("https://example.invalid/page")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	evs := obs.snapshot()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].Channel != "cache_hit" {
		t.Errorf("channel = %q, want %q", evs[0].Channel, "cache_hit")
	}
	if got := evs[0].Bytes; got != int64(len("cached response body")) {
		t.Errorf("bytes = %d, want %d", got, len("cached response body"))
	}
}

// TestClassifyOutcome covers the status→bucket mapping.
func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   EgressOutcome
	}{
		{200, nil, OutcomeSuccess},
		{299, nil, OutcomeSuccess},
		{301, nil, OutcomeRedirect},
		{404, nil, OutcomeClientError},
		{500, nil, OutcomeServerError},
		{0, ErrBlocked, OutcomeBlocked},
		{0, fmt.Errorf("wrap: %w", ErrBlocked), OutcomeBlocked},
	}
	for _, c := range cases {
		got := classifyOutcome(c.status, c.err)
		if got != c.want {
			t.Errorf("classifyOutcome(%d, %v) = %q, want %q", c.status, c.err, got, c.want)
		}
	}
}

package safehttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

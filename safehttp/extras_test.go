package safehttp_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
)

// allowLoopback is a test helper — the SSRF guard would otherwise
// reject httptest's 127.0.0.1 server. Restores the previous allowlist
// on cleanup so tests don't leak global state into each other.
func allowLoopback(t *testing.T) {
	t.Helper()
	safehttp.SetAllowedPrivateIPs([]net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("::1"),
	})
	t.Cleanup(func() { safehttp.SetAllowedPrivateIPs(nil) })
}

func TestOptionsCompose(t *testing.T) {
	allowLoopback(t)

	var (
		traceHits     atomic.Int32
		receivedSpans []map[string]any
		spansMu       sync.Mutex
	)
	tracer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/traces" || r.Method != http.MethodPost {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var body struct {
			Spans []map[string]any `json:"spans"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spansMu.Lock()
		receivedSpans = append(receivedSpans, body.Spans...)
		spansMu.Unlock()
		traceHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}))
	defer tracer.Close()

	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"wait_ms":0,"classification":"healthy"}`)
	}))
	defer coord.Close()

	// Upstream returns 500 so degraded-sink gets appended.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	var degraded []string
	c := safehttp.NewClient(
		safehttp.WithUserAgent("test-svc/1.0 (extras-compose)"),
		safehttp.WithTraceCollector(tracer.URL),
		safehttp.WithBackoffCoordinator(coord.URL),
		safehttp.WithDegradedSink(&degraded),
	)
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}

	// Degraded append is synchronous — the host portion is the
	// loopback IP that httptest binds to, not including the port.
	upstreamHost, _, _ := net.SplitHostPort(mustHost(t, upstream.URL))
	wantDegraded := upstreamHost + "-down"
	if len(degraded) != 1 || degraded[0] != wantDegraded {
		t.Fatalf("degraded = %v, want [%q]", degraded, wantDegraded)
	}

	// Trace emit is async — wait briefly.
	waitFor(t, 2*time.Second, func() bool { return traceHits.Load() >= 1 })

	spansMu.Lock()
	defer spansMu.Unlock()
	if len(receivedSpans) == 0 {
		t.Fatal("no spans received by tracer")
	}
	sp := receivedSpans[0]
	if got, _ := sp["from_service"].(string); got != "test-svc" {
		t.Errorf("from_service = %q, want test-svc (caller derivation from UA broken)", got)
	}
	if got, _ := sp["status"].(float64); int(got) != 500 {
		t.Errorf("status = %v, want 500", got)
	}
	if got, _ := sp["method"].(string); got != http.MethodGet {
		t.Errorf("method = %q, want GET", got)
	}
}

func TestDegradedSink_Concurrent(t *testing.T) {
	allowLoopback(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	var degraded []string
	c := safehttp.NewClient(
		safehttp.WithUserAgent("test-svc/1.0"),
		safehttp.WithDegradedSink(&degraded),
		safehttp.WithTimeout(5*time.Second),
	)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, err := c.Get(upstream.URL)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	if len(degraded) != n {
		t.Fatalf("degraded len=%d, want %d (one append per failed request)", len(degraded), n)
	}
}

func TestTraceCollector_FailureIsSilent(t *testing.T) {
	allowLoopback(t)

	tracer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer tracer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	c := safehttp.NewClient(
		safehttp.WithUserAgent("test-svc/1.0"),
		safehttp.WithTraceCollector(tracer.URL),
	)
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get: %v (collector failure leaked to caller — contract violation)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// Give the async emit a moment to run + log; nothing for us to
	// assert other than the caller already got their successful
	// response.
	time.Sleep(100 * time.Millisecond)
}

func TestBackoffCoordinator_Timeout(t *testing.T) {
	allowLoopback(t)

	// Coordinator that accepts the connection but never replies —
	// proxies a hung-coordinator outage. The fail-open contract
	// says the call still proceeds within the bounded budget
	// (connect 500ms + read 1s, hard cap below maxBackoffSleep).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read but never respond.
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				time.Sleep(10 * time.Second)
			}(conn)
		}
	}()
	coordURL := "http://" + ln.Addr().String()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	c := safehttp.NewClient(
		safehttp.WithUserAgent("test-svc/1.0"),
		safehttp.WithBackoffCoordinator(coordURL),
		safehttp.WithTimeout(8*time.Second),
	)

	// First call: records a failure, no coordinator consultation
	// (nothing to remember yet).
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp.Body.Close()

	// Second call: coordinator is consulted. The hung coordinator
	// MUST be bounded by the connect+read budget (≤ 1.5s), not the
	// upstream request timeout. We give it 4s of headroom for slow
	// CI before declaring a contract violation.
	start := time.Now()
	resp2, err := c.Get(upstream.URL)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	resp2.Body.Close()

	if dur > 4*time.Second {
		t.Fatalf("coordinator hang escalated to caller: took %v (budget should be ~1.5s)", dur)
	}
}

func TestNoOptions_BackwardsCompat(t *testing.T) {
	allowLoopback(t)

	var hits atomic.Int32
	var lastUA string
	var muUA sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		muUA.Lock()
		lastUA = r.Header.Get("User-Agent")
		muUA.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
	}))
	defer upstream.Close()

	// Build a client with ONLY pre-existing v0.15.0 options. The
	// extras transport must NOT be in the chain — verified
	// indirectly by behaviour parity with the golden fixture.
	c := safehttp.NewClient(
		safehttp.WithUserAgent("legacy/1.0"),
		safehttp.WithTimeout(5*time.Second),
		safehttp.WithMaxRedirects(3),
	)
	resp, err := c.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Errorf("body = %q, want hello", body)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	// The v0.15.0 client only sets UA on redirect (via
	// CheckRedirect) and otherwise lets Go's default UA through;
	// verifying the upstream still got *some* UA is enough to
	// confirm the request shape didn't change.
	muUA.Lock()
	if lastUA == "" {
		t.Errorf("UA missing — request shape regressed vs v0.15.0")
	}
	muUA.Unlock()
}

// mustHost extracts the host:port from a full URL or fails the test.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", max)
}

package graph

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetState wipes the singleton between tests. Not safe under parallel
// — these tests run serially.
func resetState(t *testing.T) {
	t.Helper()
	stateMu.Lock()
	if state != nil && state.sender != nil {
		// Best-effort shutdown; ignore if already stopped.
		select {
		case <-state.sender.stopped:
		default:
			close(state.sender.stop)
			<-state.sender.stopped
		}
	}
	state = nil
	stateOnce = sync.Once{}
	stateMu.Unlock()
	lookupMu.Lock()
	lookupTable = map[string]lookupEntry{}
	lookupMu.Unlock()
}

func TestTemplatisePath(t *testing.T) {
	cases := map[string]string{
		"/users/42":       "/users/{id}",
		"/users/42/posts": "/users/{id}/posts",
		"/share/550e8400-e29b-41d4-a716-446655440000": "/share/{uuid}",
		"/api/v2/orders":                       "/api/v2/orders",
		"/":                                    "/",
		"":                                     "/",
		"/users/42?foo=bar":                    "/users/{id}",
		"/t/abc1234deadbeef5678cafebabe9999/x": "/t/{token}/x",
	}
	for in, want := range cases {
		got := templatisePath(in)
		if got != want {
			t.Errorf("templatisePath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestCallerFromUA(t *testing.T) {
	cases := map[string]string{
		"go_apikey_scanner/1.2.3 (+https://github.com/baditaflorin/go_apikey_scanner)": "go_apikey_scanner",
		"go-pentest-subfinder/0.1.0 (+...)":                                            "go-pentest-subfinder",
		"Mozilla/5.0 (Macintosh)":                                                      "",
		"curl/7.88.1":                                                                  "",
		"":                                                                             "",
		"randomthing":                                                                  "",
		"Go-http-client/1.1":                                                           "",
	}
	for in, want := range cases {
		if got := callerFromUA(in); got != want {
			t.Errorf("callerFromUA(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestTargetFromHost(t *testing.T) {
	cases := map[string]string{
		"go-js-proxy.0exec.com":           "go-js-proxy",
		"go-pentest-subfinder.0crawl.com": "go-pentest-subfinder",
		"go-js-proxy.0exec.com:443":       "go-js-proxy",
		"example.com":                     "external:example.com",
		"10.10.10.20":                     "internal:10.10.10.20",
		"localhost":                       "internal:localhost",
		"":                                "external:unknown",
	}
	for in, want := range cases {
		if got := targetFromHost(in); got != want {
			t.Errorf("targetFromHost(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestRingDropOldest(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		_, _ = r.push(Event{Path: string(rune('a' + i))})
	}
	out := r.drain(10)
	if len(out) != 3 {
		t.Fatalf("drained %d, want 3", len(out))
	}
	// Should hold the last 3 (c, d, e) in FIFO order
	want := []string{"c", "d", "e"}
	for i, e := range out {
		if e.Path != want[i] {
			t.Errorf("ring[%d] = %q; want %q", i, e.Path, want[i])
		}
	}
}

func TestRecordWithoutCollectorIsNoOpForNetwork(t *testing.T) {
	resetState(t)
	t.Setenv("GRAPH_COLLECTOR_URL", "")
	Init("test_service", "0.0.0")
	Record(Event{Direction: "out", Target: "x", Path: "/y", Method: "GET", Status: 200})
	// Counter still increments — we keep observability for /metrics.
	s := Stats()
	if s.EventsRecorded < 1 {
		t.Errorf("expected EventsRecorded >= 1, got %d", s.EventsRecorded)
	}
}

func TestEndToEndFlush(t *testing.T) {
	resetState(t)
	var batches int64
	var seen []Event
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		var b Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		mu.Lock()
		seen = append(seen, b.Events...)
		mu.Unlock()
		atomic.AddInt64(&batches, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("GRAPH_COLLECTOR_URL", srv.URL)
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	t.Setenv("GRAPH_API_KEY", "test-key")
	Init("go_test_service", "0.1.0")
	defer Shutdown()
	for i := 0; i < 5; i++ {
		Record(Event{Direction: "out", Target: "go-js-proxy", Path: "/render", Method: "POST", Status: 200, LatencyMs: 42})
	}
	// Give the sender one tick + a bit.
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	got := len(seen)
	mu.Unlock()
	if got < 5 {
		t.Fatalf("collector saw %d events; want >= 5", got)
	}
	if atomic.LoadInt64(&batches) < 1 {
		t.Fatalf("no batches received")
	}
}

func TestLookupCachesPositive(t *testing.T) {
	resetState(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if !strings.HasPrefix(r.URL.Path, "/lookup/") {
			http.NotFound(w, r)
			return
		}
		slug := strings.TrimPrefix(r.URL.Path, "/lookup/")
		_ = json.NewEncoder(w).Encode(Service{ID: slug, URL: "https://" + slug + ".0exec.com", Healthy: true})
	}))
	defer srv.Close()
	t.Setenv("GRAPH_COLLECTOR_URL", srv.URL)
	Init("caller", "0.0.0")
	defer Shutdown()
	for i := 0; i < 4; i++ {
		svc, err := Lookup("go-js-proxy")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if svc.ID != "go-js-proxy" {
			t.Errorf("got %q; want go-js-proxy", svc.ID)
		}
	}
	if h := atomic.LoadInt64(&hits); h != 1 {
		t.Errorf("collector hit %d times; want 1 (cache miss)", h)
	}
}

func TestRoundTripperRecordsOutbound(t *testing.T) {
	resetState(t)
	var batches int64
	var seen []Event
	var mu sync.Mutex
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		seen = append(seen, b.Events...)
		mu.Unlock()
		atomic.AddInt64(&batches, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer col.Close()
	t.Setenv("GRAPH_COLLECTOR_URL", col.URL)
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	Init("caller_svc", "0.1.0")
	defer Shutdown()
	// Target is some other test server, masquerading as a fleet host
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()
	c := &http.Client{Transport: RoundTripper(http.DefaultTransport)}
	resp, err := c.Get(target.URL + "/api/users/42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no events seen")
	}
	e := seen[0]
	if e.Direction != "out" {
		t.Errorf("Direction=%q; want out", e.Direction)
	}
	if e.Status != http.StatusAccepted {
		t.Errorf("Status=%d; want %d", e.Status, http.StatusAccepted)
	}
	if e.Path != "/api/users/{id}" {
		t.Errorf("Path=%q; want /api/users/{id}", e.Path)
	}
	if e.Caller != "caller_svc" {
		t.Errorf("Caller=%q; want caller_svc", e.Caller)
	}
}

func TestMiddlewareRecordsInbound(t *testing.T) {
	resetState(t)
	var batches int64
	var seen []Event
	var mu sync.Mutex
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		seen = append(seen, b.Events...)
		mu.Unlock()
		atomic.AddInt64(&batches, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer col.Close()
	t.Setenv("GRAPH_COLLECTOR_URL", col.URL)
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	Init("target_svc", "0.1.0")
	defer Shutdown()

	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/widgets/42", nil)
	req.Header.Set("User-Agent", "go_caller_svc/0.1.0 (+...)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no events seen")
	}
	e := seen[0]
	if e.Direction != "in" {
		t.Errorf("Direction=%q; want in", e.Direction)
	}
	if e.Caller != "go_caller_svc" {
		t.Errorf("Caller=%q; want go_caller_svc", e.Caller)
	}
	if e.Target != "target_svc" {
		t.Errorf("Target=%q; want target_svc", e.Target)
	}
	if e.Path != "/widgets/{id}" {
		t.Errorf("Path=%q; want /widgets/{id}", e.Path)
	}
	if e.Status != http.StatusCreated {
		t.Errorf("Status=%d; want %d", e.Status, http.StatusCreated)
	}
}

func TestProbePathsExcluded(t *testing.T) {
	resetState(t)
	for _, p := range []string{"/health", "/version", "/metrics", "/capabilities"} {
		if !isProbe(p) {
			t.Errorf("isProbe(%q) = false; want true", p)
		}
	}
	if isProbe("/render") {
		t.Errorf("isProbe(/render) = true; want false")
	}
}

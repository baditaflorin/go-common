package graph

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestSender(cfg config, batchSize int) (*sender, *ring, *atomicCounters) {
	r := newRing(16)
	counters := &atomicCounters{}
	if batchSize > 0 {
		cfg.flushBatch = batchSize
	}
	return newSender(cfg, "go-test-sender", "test", r, counters), r, counters
}

func TestGraphEventEmissionIsExplicitAndCredentialScoped(t *testing.T) {
	cases := []struct {
		name          string
		enabled       string
		collectorURL  string
		writerAPIKey  string
		fleetAPIKey   string
		wantEmitting  bool
		wantConfigErr bool
	}{
		{
			name: "default disabled",
		},
		{
			name:         "fleet key is never a writer fallback",
			enabled:      "true",
			collectorURL: "https://fleet-graph.0exec.com",
			fleetAPIKey:  "broad-fleet-key",
		},
		{
			name:         "complete explicit writer configuration",
			enabled:      "true",
			collectorURL: "https://fleet-graph.0exec.com/",
			writerAPIKey: "writer-key",
			wantEmitting: true,
		},
		{
			name:          "noncanonical HTTPS host is rejected",
			enabled:       "true",
			collectorURL:  "https://collector.example.test",
			writerAPIKey:  "writer-key",
			wantConfigErr: true,
		},
		{
			name:          "non-loopback HTTP is rejected",
			enabled:       "true",
			collectorURL:  "http://collector.example.test",
			writerAPIKey:  "writer-key",
			wantConfigErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GRAPH_ENABLED", tc.enabled)
			t.Setenv("GRAPH_COLLECTOR_URL", tc.collectorURL)
			t.Setenv("GRAPH_API_KEY", tc.writerAPIKey)
			t.Setenv("GRAPH_READER_API_KEY", "")
			t.Setenv("FLEET_API_KEY", tc.fleetAPIKey)

			cfg := loadConfig()
			if got := cfg.eventEmissionEnabled(); got != tc.wantEmitting {
				t.Fatalf("eventEmissionEnabled() = %v, want %v", got, tc.wantEmitting)
			}
			if (cfg.collectorErr != nil) != tc.wantConfigErr {
				t.Fatalf("collector error = %v, want config error=%v", cfg.collectorErr, tc.wantConfigErr)
			}
		})
	}
}

func TestNormalizeCollectorURLTransportPolicy(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "empty", raw: "", want: "", ok: true},
		{name: "canonical remote HTTPS", raw: "https://fleet-graph.0exec.com/", want: "https://fleet-graph.0exec.com", ok: true},
		{name: "canonical remote default port", raw: "https://FLEET-GRAPH.0EXEC.COM:443", want: "https://fleet-graph.0exec.com", ok: true},
		{name: "loopback IPv4 HTTP", raw: "http://127.0.0.1:8090", want: "http://127.0.0.1:8090", ok: true},
		{name: "loopback IPv6 HTTP", raw: "http://[::1]:8090", want: "http://[::1]:8090", ok: true},
		{name: "localhost HTTP", raw: "http://localhost:8090", want: "http://localhost:8090", ok: true},
		{name: "remote HTTP", raw: "http://fleet-graph.0exec.com", ok: false},
		{name: "noncanonical HTTPS host", raw: "https://collector.example.test", ok: false},
		{name: "nondefault remote HTTPS port", raw: "https://fleet-graph.0exec.com:8443", ok: false},
		{name: "path", raw: "https://fleet-graph.0exec.com/v1", ok: false},
		{name: "query", raw: "https://fleet-graph.0exec.com/?trace=1", ok: false},
		{name: "userinfo", raw: "https://user@example.test", ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeCollectorURL(tc.raw)
			if (err == nil) != tc.ok {
				t.Fatalf("normalizeCollectorURL(%q) error = %v, want success=%v", tc.raw, err, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("normalizeCollectorURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSenderDoesNotSendWithoutDedicatedWriterKey(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", srv.URL)
	t.Setenv("GRAPH_API_KEY", "")
	t.Setenv("FLEET_API_KEY", "broad-fleet-key")
	cfg := loadConfig()
	s, r, _ := newTestSender(cfg, 1)
	_, _ = r.push(Event{Path: "/one"})
	s.flush()

	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("collector received %d requests without GRAPH_API_KEY; want 0", got)
	}
	if got := r.len(); got != 1 {
		t.Fatalf("ring length = %d after non-emitting flush; want 1", got)
	}
}

func TestSenderStopsFlushAfterFirstFailedBatch(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
	s, r, counters := newTestSender(cfg, 1)
	for i := 0; i < 3; i++ {
		_, _ = r.push(Event{Path: "/event"})
	}
	s.flush()

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("collector received %d batches; want exactly one after first failure", got)
	}
	if got := r.len(); got != 2 {
		t.Fatalf("ring length = %d after first failed batch; want 2 pending events", got)
	}
	if got := len(s.pending); got != 1 {
		t.Fatalf("failed batch length = %d, want 1 preserved event", got)
	}
	if got := atomic.LoadInt64(&counters.BatchesFailed); got != 1 {
		t.Fatalf("BatchesFailed = %d, want 1", got)
	}
}

func TestSenderAuthFailuresCooldown(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&hits, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
			s, r, counters := newTestSender(cfg, 1)
			_, _ = r.push(Event{Path: "/one"})
			_, _ = r.push(Event{Path: "/two"})
			before := time.Now()
			s.flush()

			if got := atomic.LoadInt64(&hits); got != 1 {
				t.Fatalf("collector received %d batches; want 1", got)
			}
			until := time.Unix(0, atomic.LoadInt64(&s.authCooldownUntil))
			if remaining := until.Sub(before); remaining < graphAuthFailureCooldown-2*time.Second || remaining > graphAuthFailureCooldown+time.Second {
				t.Fatalf("auth cooldown = %s, want approximately %s", remaining, graphAuthFailureCooldown)
			}
			s.flush()
			if got := atomic.LoadInt64(&hits); got != 1 {
				t.Fatalf("collector received %d batches during cooldown; want 1", got)
			}
			if got := r.len(); got != 1 {
				t.Fatalf("ring length = %d during cooldown; want 1 pending event", got)
			}
			if got := len(s.pending); got != 1 {
				t.Fatalf("failed batch length = %d during cooldown; want 1 preserved event", got)
			}
			if got := atomic.LoadInt64(&counters.BatchesFailed); got != 1 {
				t.Fatalf("BatchesFailed = %d, want 1", got)
			}
		})
	}
}

func TestSenderDoesNotFollowRedirects(t *testing.T) {
	var sourceHits, redirectedHits int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&redirectedHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&sourceHits, 1)
		w.Header().Set("Location", redirected.URL+"/events")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	cfg := config{enabled: true, collectorURL: source.URL, writerAPIKey: "writer-key"}
	s, r, counters := newTestSender(cfg, 1)
	_, _ = r.push(Event{Path: "/one"})
	s.flush()

	if got := atomic.LoadInt64(&sourceHits); got != 1 {
		t.Fatalf("redirecting collector received %d requests, want 1", got)
	}
	if got := atomic.LoadInt64(&redirectedHits); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0", got)
	}
	if got := atomic.LoadInt64(&counters.BatchesFailed); got != 1 {
		t.Fatalf("BatchesFailed = %d, want 1 redirect failure", got)
	}
}

func TestLookupRequiresDedicatedReaderKeyWithoutNetwork(t *testing.T) {
	resetState(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("GRAPH_COLLECTOR_URL", srv.URL)
	t.Setenv("GRAPH_API_KEY", "writer-key")
	t.Setenv("GRAPH_READER_API_KEY", "")
	Init("lookup-caller", "test")
	defer Shutdown()

	_, err := Lookup("go-target")
	if !errors.Is(err, ErrReaderKeyNotConfigured) {
		t.Fatalf("Lookup error = %v, want ErrReaderKeyNotConfigured", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("lookup made %d requests without GRAPH_READER_API_KEY; want 0", got)
	}
}

func TestLookupDoesNotFollowRedirects(t *testing.T) {
	resetState(t)
	var sourceHits, redirectedHits int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&redirectedHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&sourceHits, 1)
		w.Header().Set("Location", redirected.URL+"/lookup/go-target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	t.Setenv("GRAPH_COLLECTOR_URL", source.URL)
	t.Setenv("GRAPH_READER_API_KEY", "reader-key")
	Init("lookup-caller", "test")
	defer Shutdown()

	_, err := Lookup("go-target")
	if err == nil {
		t.Fatal("Lookup succeeded through a redirect; want a non-2xx error")
	}
	if got := atomic.LoadInt64(&sourceHits); got != 1 {
		t.Fatalf("redirecting collector received %d requests, want 1", got)
	}
	if got := atomic.LoadInt64(&redirectedHits); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0", got)
	}
}

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMTLSClient stands in for *spiffe.HTTPClient: it sends with the given
// client (an httptest TLS server's), or fails every request with err.
type fakeMTLSClient struct {
	client *http.Client
	err    error
	calls  atomic.Int64
	closed atomic.Bool
}

func (f *fakeMTLSClient) Do(req *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.client.Do(req)
}

func (f *fakeMTLSClient) Close() error {
	f.closed.Store(true)
	return nil
}

// recordingCollector answers POST /events with status and records what came.
type recordingCollector struct {
	mu       sync.Mutex
	status   int
	requests []recordedRequest
}

type recordedRequest struct {
	path, apiKey string
	batch        Batch
}

func (c *recordingCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b Batch
	_ = json.NewDecoder(r.Body).Decode(&b)
	c.mu.Lock()
	c.requests = append(c.requests, recordedRequest{path: r.URL.Path, apiKey: r.Header.Get("X-API-Key"), batch: b})
	status := c.status
	c.mu.Unlock()
	w.WriteHeader(status)
}

func (c *recordingCollector) got() []recordedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedRequest(nil), c.requests...)
}

// resetMTLSFactory clears the package's factory for this test and the next.
func resetMTLSFactory(t *testing.T) {
	t.Helper()
	reset := func() {
		mtlsFactoryMu.Lock()
		mtlsFactory = nil
		mtlsFactoryReady = make(chan struct{})
		mtlsFactoryMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// twoPathSender has a writer-key collector and an mTLS collector, with the
// mTLS client already connected (as connectMTLS leaves it).
func twoPathSender(t *testing.T, mtlsStatus, keyStatus int) (*sender, *ring, *atomicCounters, *recordingCollector, *recordingCollector, *fakeMTLSClient) {
	t.Helper()
	mtlsCollector := &recordingCollector{status: mtlsStatus}
	mtlsSrv := httptest.NewTLSServer(mtlsCollector)
	t.Cleanup(mtlsSrv.Close)
	keyCollector := &recordingCollector{status: keyStatus}
	keySrv := httptest.NewServer(keyCollector)
	t.Cleanup(keySrv.Close)
	cfg := config{
		enabled: true, collectorURL: keySrv.URL, writerAPIKey: "writer-test-key",
		spiffeCollectorURL: mtlsSrv.URL, spiffeCollectorID: "spiffe://0exec.com/ns/fleet/sa/fleet-graph",
		spiffeSocket: "unix:///run/spire/sockets/agent.sock",
	}
	s, r, counters := newTestSender(cfg, 1)
	client := &fakeMTLSClient{client: mtlsSrv.Client()}
	if !s.setMTLSClient(client) {
		t.Fatal("setMTLSClient refused a client before shutdown")
	}
	return s, r, counters, mtlsCollector, keyCollector, client
}

func TestNormalizeSPIFFECollectorURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                             "",
		"https://go-fleet-graph:8443":  "https://go-fleet-graph:8443",
		"HTTPS://Go-Fleet-Graph:8443/": "https://go-fleet-graph:8443",
		" https://10.0.0.5:8443 ":      "https://10.0.0.5:8443",
	} {
		got, err := normalizeSPIFFECollectorURL(raw)
		if err != nil || got != want {
			t.Errorf("normalizeSPIFFECollectorURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"http://go-fleet-graph:8443", "go-fleet-graph:8443", "https://", "https://u:p@go-fleet-graph:8443",
		"https://go-fleet-graph:8443/events", "https://go-fleet-graph:8443?x=1", "https://go-fleet-graph:8443#f",
	} {
		if got, err := normalizeSPIFFECollectorURL(raw); err == nil {
			t.Errorf("normalizeSPIFFECollectorURL(%q) = %q, nil; want an error", raw, got)
		}
	}
}

func TestSPIFFEPathNeedsAllThreeSettings(t *testing.T) {
	full := config{
		enabled: true, spiffeCollectorURL: "https://go-fleet-graph:8443",
		spiffeCollectorID: "spiffe://0exec.com/ns/fleet/sa/fleet-graph", spiffeSocket: "unix:///run/spire/sockets/agent.sock",
	}
	if !full.spiffeConfigured() || !full.eventEmissionConfigured() || !full.eventEmissionEnabled() {
		t.Fatal("the SPIFFE path with all three settings and no writer key must enable emission")
	}
	for name, mutate := range map[string]func(*config){
		"no url":       func(c *config) { c.spiffeCollectorURL = "" },
		"bad url":      func(c *config) { c.spiffeCollectorErr = errors.New("bad") },
		"no id":        func(c *config) { c.spiffeCollectorID = "" },
		"not an id":    func(c *config) { c.spiffeCollectorID = "fleet-graph" },
		"no socket":    func(c *config) { c.spiffeSocket = "" },
		"not enabled":  func(c *config) { c.enabled = false },
		"id has space": func(c *config) { c.spiffeCollectorID = "spiffe://0exec.com/ns/fleet/sa/fleet graph" },
	} {
		c := full
		mutate(&c)
		if c.eventEmissionEnabled() {
			t.Errorf("%s: emission enabled with only part of the SPIFFE path and no writer key", name)
		}
	}
}

func TestSPIFFEEnvIsRead(t *testing.T) {
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_SPIFFE_COLLECTOR_URL", "https://go-fleet-graph:8443")
	t.Setenv("GRAPH_SPIFFE_COLLECTOR_ID", "spiffe://0exec.com/ns/fleet/sa/fleet-graph")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "unix:///run/spire/sockets/agent.sock")
	c := loadConfig()
	if !c.spiffeConfigured() || c.keyPathConfigured() || !c.eventEmissionEnabled() {
		t.Fatalf("loadConfig: spiffe=%v key=%v enabled=%v; want the SPIFFE path alone", c.spiffeConfigured(), c.keyPathConfigured(), c.eventEmissionEnabled())
	}
}

func TestSenderUsesSPIFFEPathWithoutTheKey(t *testing.T) {
	s, r, counters, mtls, key, _ := twoPathSender(t, http.StatusOK, http.StatusOK)
	_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
	s.flush()

	got := mtls.got()
	if len(got) != 1 || len(key.got()) != 0 {
		t.Fatalf("mTLS collector got %d, key collector %d; want 1 and 0", len(got), len(key.got()))
	}
	if got[0].apiKey != "" {
		t.Fatal("the writer key was sent on the SPIFFE path")
	}
	if got[0].path != "/events" || got[0].batch.Service != "go-test-sender" || got[0].batch.Events[0].Caller != "go-test-sender" {
		t.Fatalf("request = %+v; want POST /events for go-test-sender", got[0])
	}
	st := counters.snapshot()
	if st.BatchesSent != 1 || st.BatchesSentSPIFFE != 1 || st.BatchesFailed != 0 || st.BatchesSPIFFEFallback != 0 {
		t.Fatalf("counters = %+v; want one batch sent over SPIFFE", st)
	}
}

func TestSPIFFEPathFailureFallsBackToTheKeyAndCoolsDown(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusMisdirectedRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, r, counters, mtls, key, _ := twoPathSender(t, status, http.StatusOK)
			_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
			s.flush()
			if len(mtls.got()) != 1 || len(key.got()) != 1 {
				t.Fatalf("mTLS collector got %d, key collector %d; want the batch on both", len(mtls.got()), len(key.got()))
			}
			if key.got()[0].apiKey != "writer-test-key" {
				t.Fatal("the fallback did not carry the writer key")
			}
			// Inside the cool-down the next batch goes straight to the key.
			_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
			s.flush()
			if len(mtls.got()) != 1 || len(key.got()) != 2 {
				t.Fatalf("after the cool-down started: mTLS %d, key %d; want 1 and 2", len(mtls.got()), len(key.got()))
			}
			st := counters.snapshot()
			if st.BatchesSent != 2 || st.BatchesSentSPIFFE != 0 || st.BatchesSPIFFEFallback != 1 || st.BatchesFailed != 0 {
				t.Fatalf("counters = %+v; want 2 sent with the key, 1 of them a fallback, none failed", st)
			}
			// After the cool-down the SPIFFE path is tried again.
			s.mtlsMu.Lock()
			s.mtlsCooldownUntil = time.Now().Add(-time.Second)
			s.mtlsMu.Unlock()
			_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
			s.flush()
			if len(mtls.got()) != 2 {
				t.Fatalf("after the cool-down the mTLS collector got %d; want 2", len(mtls.got()))
			}
		})
	}
}

func TestSPIFFETransportErrorFallsBackToTheKey(t *testing.T) {
	s, r, counters, _, key, client := twoPathSender(t, http.StatusOK, http.StatusOK)
	client.err = errors.New("tls: handshake failure")
	_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
	s.flush()
	if client.calls.Load() != 1 || len(key.got()) != 1 {
		t.Fatalf("mTLS attempts %d, key collector %d; want 1 and 1", client.calls.Load(), len(key.got()))
	}
	if st := counters.snapshot(); st.BatchesSent != 1 || st.BatchesSPIFFEFallback != 1 {
		t.Fatalf("counters = %+v; want the batch sent with the key as a fallback", st)
	}
}

// An answer from the collector is about the batch, not the path: the key
// would reach the same collector, so the batch is not sent twice.
func TestSPIFFECollectorAnswerIsFinal(t *testing.T) {
	for status, want := range map[int]sendResult{
		http.StatusServiceUnavailable:    sendRetain,
		http.StatusInternalServerError:   sendRetain,
		http.StatusTooManyRequests:       sendRetain,
		http.StatusBadRequest:            sendRejected,
		http.StatusRequestEntityTooLarge: sendRejected,
		http.StatusForbidden:             sendRejected,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, _, counters, mtls, key, _ := twoPathSender(t, status, http.StatusOK)
			s.identityMu.Lock()
			s.serviceID = "go-test-sender"
			s.identityMu.Unlock()
			if got := s.send(context.Background(), []Event{{Direction: "out", Target: "technology-stack"}}); got != want {
				t.Fatalf("send = %v, want %v", got, want)
			}
			if len(mtls.got()) != 1 || len(key.got()) != 0 {
				t.Fatalf("mTLS collector got %d, key collector %d; want 1 and 0", len(mtls.got()), len(key.got()))
			}
			st := counters.snapshot()
			if st.BatchesFailed != 1 || st.BatchesSPIFFEFallback != 0 || (want == sendRejected) != (st.BatchesRejected == 1) {
				t.Fatalf("counters = %+v for status %d", st, status)
			}
		})
	}
}

func TestSPIFFEOnlySenderKeepsTheBatchWhenThePathFails(t *testing.T) {
	s, r, counters, _, key, client := twoPathSender(t, http.StatusOK, http.StatusOK)
	s.cfg.writerAPIKey = ""
	s.cfg.collectorURL = ""
	client.err = errors.New("connection refused")
	_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
	s.flush()
	if len(key.got()) != 0 || len(s.pending) != 1 {
		t.Fatalf("key collector got %d, pending %d; want 0 and the batch kept", len(key.got()), len(s.pending))
	}
	if st := counters.snapshot(); st.BatchesFailed != 1 || st.BatchesSPIFFEFallback != 0 {
		t.Fatalf("counters = %+v; want one failed batch and no fallback", st)
	}
	// While the path cools down and there is no key, nothing is attempted.
	if s.canWriteAt(time.Now()) {
		t.Fatal("canWriteAt is true with the only path cooling down")
	}
}

// A 401 on the key path pauses the key, not the SPIFFE path.
func TestKeyCooldownDoesNotStopTheSPIFFEPath(t *testing.T) {
	s, r, _, mtls, _, _ := twoPathSender(t, http.StatusOK, http.StatusOK)
	atomic.StoreInt64(&s.authCooldownUntil, time.Now().Add(time.Hour).UnixNano())
	_, _ = r.push(Event{Direction: "out", Target: "technology-stack"})
	s.flush()
	if len(mtls.got()) != 1 {
		t.Fatalf("mTLS collector got %d; want the batch while the key cools down", len(mtls.got()))
	}
}

func TestSenderConnectsThroughTheFactoryAndClosesOnShutdown(t *testing.T) {
	resetState(t)
	resetMTLSFactory(t)
	mtls := &recordingCollector{status: http.StatusOK}
	srv := httptest.NewTLSServer(mtls)
	defer srv.Close()
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	t.Setenv("GRAPH_SPIFFE_COLLECTOR_URL", srv.URL)
	t.Setenv("GRAPH_SPIFFE_COLLECTOR_ID", "spiffe://0exec.com/ns/fleet/sa/fleet-graph")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "unix:///run/spire/sockets/agent.sock")
	Init("caller", "test")
	if !Enabled() {
		t.Fatal("Enabled() = false with the SPIFFE path configured and no key")
	}

	type call struct {
		socket  string
		ids     []string
		timeout time.Duration
	}
	calls := make(chan call, 4)
	client := &fakeMTLSClient{client: srv.Client()}
	// The factory arrives after the sender started, as with a Record before server.New.
	SetMTLSClientFactory(func(_ context.Context, socket string, ids []string, timeout time.Duration) (MTLSClient, error) {
		calls <- call{socket, ids, timeout}
		return client, nil
	})
	SetMTLSClientFactory(func(context.Context, string, []string, time.Duration) (MTLSClient, error) {
		t.Error("a second factory replaced the first")
		return nil, errors.New("second")
	})
	select {
	case c := <-calls:
		if c.socket != "unix:///run/spire/sockets/agent.sock" || len(c.ids) != 1 || c.ids[0] != "spiffe://0exec.com/ns/fleet/sa/fleet-graph" || c.timeout != sendTimeout {
			t.Fatalf("factory called with %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sender never called the factory")
	}
	Record(Event{Direction: "out", Target: "technology-stack", Method: http.MethodGet})
	deadline := time.Now().Add(5 * time.Second)
	for len(mtls.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := mtls.got(); len(got) != 1 || got[0].apiKey != "" || got[0].batch.Service != "caller" {
		t.Fatalf("mTLS collector got %+v; want one keyless batch from caller", got)
	}
	if err := Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !client.closed.Load() {
		t.Fatal("Shutdown did not close the mTLS client")
	}
}

func TestConnectRetriesUntilTheFactorySucceeds(t *testing.T) {
	resetMTLSFactory(t)
	var attempts atomic.Int64
	client := &fakeMTLSClient{}
	SetMTLSClientFactory(func(context.Context, string, []string, time.Duration) (MTLSClient, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("workload identity unavailable")
		}
		return client, nil
	})
	cfg := config{enabled: true, spiffeCollectorURL: "https://go-fleet-graph:8443",
		spiffeCollectorID: "spiffe://0exec.com/ns/fleet/sa/fleet-graph", spiffeSocket: "unix:///run/spire/sockets/agent.sock"}
	s, _, _ := newTestSender(cfg, 1)
	s.mtlsRetryPause = 10 * time.Millisecond
	go s.connectMTLS()
	select {
	case <-s.mtlsConnected:
	case <-time.After(5 * time.Second):
		t.Fatal("connectMTLS did not retry after the first failure")
	}
	if attempts.Load() != 2 || s.mtlsClientAt(time.Now()) != client {
		t.Fatalf("attempts = %d, client set = %v; want 2 and the client", attempts.Load(), s.mtlsClientAt(time.Now()) == client)
	}
}

func TestConnectStopsWithTheSender(t *testing.T) {
	resetMTLSFactory(t)
	cfg := config{enabled: true, spiffeCollectorURL: "https://go-fleet-graph:8443",
		spiffeCollectorID: "spiffe://0exec.com/ns/fleet/sa/fleet-graph", spiffeSocket: "unix:///run/spire/sockets/agent.sock"}
	s, _, _ := newTestSender(cfg, 1)
	go s.connectMTLS() // no factory is ever installed
	close(s.stop)
	select {
	case <-s.mtlsConnected:
	case <-time.After(time.Second):
		t.Fatal("connectMTLS kept waiting for a factory after the sender stopped")
	}
}

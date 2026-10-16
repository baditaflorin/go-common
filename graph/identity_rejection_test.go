package graph

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// collectorStub records every batch it receives and answers with the next
// scripted status (200 once the script is exhausted).
type collectorStub struct {
	mu      sync.Mutex
	script  []int
	batches []Batch
}

func (c *collectorStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		c.mu.Lock()
		c.batches = append(c.batches, b)
		status := http.StatusOK
		if len(c.script) > 0 {
			status, c.script = c.script[0], c.script[1:]
		}
		c.mu.Unlock()
		w.WriteHeader(status)
	})
}

func (c *collectorStub) received() []Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Batch(nil), c.batches...)
}

func TestSenderDropsBatchTheCollectorRejects(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusForbidden, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stub := &collectorStub{script: []int{status}}
			srv := httptest.NewServer(stub.handler(t))
			defer srv.Close()
			cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
			s, r, counters := newTestSender(cfg, 1)
			_, _ = r.push(Event{Direction: "out", Caller: "go-test-sender", Path: "/bad"})
			_, _ = r.push(Event{Direction: "out", Caller: "go-test-sender", Path: "/good"})

			s.flush()
			if len(s.pending) != 0 {
				t.Fatalf("rejected batch still pending: %+v", s.pending)
			}
			if got := atomic.LoadInt64(&counters.BatchesRejected); got != 1 {
				t.Fatalf("BatchesRejected = %d, want 1", got)
			}
			if got := atomic.LoadInt64(&counters.BatchesFailed); got != 1 {
				t.Fatalf("BatchesFailed = %d, want 1 (rejected is a subset of failed)", got)
			}
			if got := r.len(); got != 1 {
				t.Fatalf("ring length = %d after a rejected batch; want the later event untouched", got)
			}

			// The next tick delivers the later batch instead of resending the
			// rejected one. 403 also starts the auth cooldown; lift it.
			atomic.StoreInt64(&s.authCooldownUntil, 0)
			s.flush()
			got := stub.received()
			if len(got) != 2 {
				t.Fatalf("collector saw %d batches, want 2", len(got))
			}
			if len(got[1].Events) != 1 || got[1].Events[0].Path != "/good" {
				t.Fatalf("second batch = %+v, want only the later /good event", got[1].Events)
			}
		})
	}
}

func TestSenderRetainsBatchOnRetryableStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stub := &collectorStub{script: []int{status}}
			srv := httptest.NewServer(stub.handler(t))
			defer srv.Close()
			cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
			s, r, counters := newTestSender(cfg, 1)
			_, _ = r.push(Event{Direction: "out", Caller: "go-test-sender", Path: "/keep"})

			s.flush()
			if len(s.pending) != 1 {
				t.Fatalf("pending = %d, want the batch kept for a later flush", len(s.pending))
			}
			if got := atomic.LoadInt64(&counters.BatchesRejected); got != 0 {
				t.Fatalf("BatchesRejected = %d, want 0", got)
			}

			atomic.StoreInt64(&s.authCooldownUntil, 0)
			s.flush()
			got := stub.received()
			if len(got) != 2 || len(got[1].Events) != 1 || got[1].Events[0].Path != "/keep" {
				t.Fatalf("retry batches = %+v, want the same /keep batch resent", got)
			}
			if len(s.pending) != 0 {
				t.Fatalf("pending = %d after delivery, want 0", len(s.pending))
			}
		})
	}
}

// A collector that checks the batch like validateWriterBatch does: every
// outbound event's caller must be the authenticated writer.
func TestRejectedPoisonBatchNoLongerPinsLaterBatches(t *testing.T) {
	const writer = "go-test-sender"
	var accepted int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b Batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, e := range b.Events {
			if e.Direction == "out" && e.Caller != writer {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		atomic.AddInt64(&accepted, int64(len(b.Events)))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
	s, r, _ := newTestSender(cfg, 1)
	_, _ = r.push(Event{Direction: "out", Caller: "someone-else", Path: "/poison"})
	for i := 0; i < 3; i++ {
		_, _ = r.push(Event{Direction: "out", Caller: writer, Path: "/ok"})
	}
	for i := 0; i < 4; i++ {
		atomic.StoreInt64(&s.authCooldownUntil, 0)
		s.flush()
	}
	if got := atomic.LoadInt64(&accepted); got != 3 {
		t.Fatalf("collector accepted %d events, want the 3 good ones delivered after the poison batch was dropped", got)
	}
}

func TestStampOwnIdentity(t *testing.T) {
	events := []Event{
		{Direction: "out", Caller: "", Target: "x"},
		{Direction: "out", Caller: "unknown", Target: "x"},
		{Direction: "out", Caller: "kept", Target: "x"},
		{Direction: "in", Caller: "unknown", Target: ""},
		{Direction: "in", Caller: "unknown", Target: "unknown"},
		{Direction: "in", Caller: "", Target: "kept"},
	}
	stampOwnIdentity(events, "svc")
	want := []Event{
		{Direction: "out", Caller: "svc", Target: "x"},
		{Direction: "out", Caller: "svc", Target: "x"},
		{Direction: "out", Caller: "kept", Target: "x"},
		{Direction: "in", Caller: "unknown", Target: "svc"},
		{Direction: "in", Caller: "unknown", Target: "svc"},
		{Direction: "in", Caller: "", Target: "kept"},
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func setGraphEnv(t *testing.T, collectorURL string) {
	t.Helper()
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", collectorURL)
	t.Setenv("GRAPH_API_KEY", "writer-test-key")
	t.Setenv("GRAPH_SERVICE_ID", "")
}

func TestEventsRecordedBeforeInitAreStampedWithTheRealIdentity(t *testing.T) {
	resetState(t)
	stub := &collectorStub{}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	setGraphEnv(t, srv.URL)

	// A secrets lookup or poller that runs before server.New: the identity is
	// not known yet, and the outbound transport must still observe the call.
	if got := ServiceID(); got != "" {
		t.Fatalf("ServiceID() before Init = %q, want empty (not the placeholder %q)", got, unknownIdentity)
	}
	if !Enabled() {
		t.Fatal("Enabled() = false before Init with a complete writer config; pre-Init calls would go unobserved")
	}
	rt := RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://fleet-secrets.0exec.com/secrets/x", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	Record(Event{Direction: "in", Caller: "unknown", Path: "/probe", Method: http.MethodGet, Status: 200})
	if got := Stats().EventsRecorded; got != 2 {
		t.Fatalf("EventsRecorded = %d before Init, want 2 held", got)
	}

	// Nothing may leave under a placeholder identity while it is unknown.
	ensureInit().sender.flush()
	if got := stub.received(); len(got) != 0 {
		t.Fatalf("collector received %d batches before the identity was known: %+v", len(got), got)
	}
	if got := ensureInit().ring.len(); got != 2 {
		t.Fatalf("ring length = %d, want both events still held", got)
	}

	Init("canonical-svc", "1.2.3")
	defer Shutdown()
	ensureInit().sender.flush()

	got := stub.received()
	if len(got) != 1 || got[0].Service != "canonical-svc" || len(got[0].Events) != 2 {
		t.Fatalf("batches = %+v, want one canonical-svc batch with both held events", got)
	}
	out, in := got[0].Events[0], got[0].Events[1]
	if out.Direction != "out" || out.Caller != "canonical-svc" || out.Target == "" {
		t.Fatalf("held outbound event = %+v, want caller stamped with the real identity", out)
	}
	if in.Direction != "in" || in.Target != "canonical-svc" || in.Caller != "unknown" {
		t.Fatalf("held inbound event = %+v, want target stamped and the unverified caller left as is", in)
	}
}

func TestBootstrapResolvesGraphServiceIDBeforeInit(t *testing.T) {
	resetState(t)
	stub := &collectorStub{}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	setGraphEnv(t, srv.URL)
	t.Setenv("GRAPH_SERVICE_ID", "Pentest-Triager-Listener")

	Record(Event{Direction: "out", Target: "fleet-secrets", Path: "/secrets/x", Method: http.MethodGet, Status: 200})
	if got := ServiceID(); got != "pentest-triager-listener" {
		t.Fatalf("ServiceID() before Init = %q, want the canonical override", got)
	}
	ensureInit().sender.flush()
	defer Shutdown()

	got := stub.received()
	if len(got) != 1 || got[0].Service != "pentest-triager-listener" ||
		len(got[0].Events) != 1 || got[0].Events[0].Caller != "pentest-triager-listener" {
		t.Fatalf("batches = %+v, want the pre-Init event sent under the canonical identity", got)
	}
}

func TestInvalidGraphServiceIDBeforeInitFailsClosed(t *testing.T) {
	resetState(t)
	setGraphEnv(t, "https://fleet-graph.0exec.com")
	t.Setenv("GRAPH_SERVICE_ID", "not_a_label")

	if Enabled() {
		t.Fatal("Enabled() = true with an invalid GRAPH_SERVICE_ID before Init")
	}
	Record(Event{Direction: "out", Target: "example.com", Method: http.MethodGet, Status: 200})
	if got := Stats().EventsRecorded; got != 0 {
		t.Fatalf("recorded %d events with an invalid service identity, want none", got)
	}
}

// The incident path: a service records (or asks Enabled) before Init, so the
// bootstrap is what resolves the invalid GRAPH_SERVICE_ID. It must still log
// once and count, and a later Init must not log a second line.
func TestInvalidGraphServiceIDBeforeInitIsLoggedOnceAndCounted(t *testing.T) {
	resetState(t)
	buf := captureLog(t)
	setGraphEnv(t, "https://fleet-graph.0exec.com")
	t.Setenv("GRAPH_SERVICE_ID", "not_a_label")

	if Enabled() {
		t.Fatal("Enabled() = true with an invalid GRAPH_SERVICE_ID before Init")
	}
	if got := len(logLines(buf)); got != 1 {
		t.Fatalf("got %d log lines from the bootstrap, want 1:\n%s", got, buf.String())
	}
	if got := Stats().EventsDroppedInvalidIdentity; got != 0 {
		t.Fatalf("Enabled() counted %d drops, want 0 (it is a query)", got)
	}
	Record(Event{Direction: "out", Target: "example.com", Method: http.MethodGet, Status: 200})
	Record(Event{Direction: "in", Caller: "x", Method: http.MethodGet, Status: 200})
	Init("go_domain_example", "1.0.0")
	defer Shutdown()
	if got := Stats().EventsDroppedInvalidIdentity; got != 2 {
		t.Fatalf("EventsDroppedInvalidIdentity = %d, want 2", got)
	}
	if got := Stats().EventsRecorded; got != 0 {
		t.Fatalf("recorded %d events with an invalid identity, want none", got)
	}
	if got := len(logLines(buf)); got != 1 {
		t.Fatalf("got %d log lines after Init, want still 1:\n%s", got, buf.String())
	}
}

func TestEnabledStaysOffBeforeInitWithoutWriterConfig(t *testing.T) {
	resetState(t)
	t.Setenv("GRAPH_ENABLED", "")
	t.Setenv("GRAPH_SERVICE_ID", "")
	if Enabled() {
		t.Fatal("Enabled() = true without explicit writer configuration")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

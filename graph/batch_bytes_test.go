package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func evs(n int, path string) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = Event{Direction: "out", Caller: "a", Target: "b", Path: path, Method: "GET", Status: 200}
	}
	return out
}

func TestShortEventsFillABatchOfTwoThousand(t *testing.T) {
	if got := loadConfig().flushBatch; got != 2000 {
		t.Fatalf("default flush batch = %d, want 2000", got)
	}
	head, rest := splitAtBytes(evs(2000, "/v1/{id}"), maxBatchBytes)
	if len(head) != 2000 || len(rest) != 0 {
		t.Fatalf("2000 short events were split: head=%d rest=%d", len(head), len(rest))
	}
}

func TestLongPathsAreCutAtTheByteBound(t *testing.T) {
	events := evs(2000, "/"+strings.Repeat("p", 600))
	head, rest := splitAtBytes(events, maxBatchBytes)
	if len(head) == 0 || len(rest) == 0 || len(head)+len(rest) != 2000 {
		t.Fatalf("head=%d rest=%d, want a split of 2000", len(head), len(rest))
	}
	size := 0
	for _, e := range head {
		size += eventBytes(e)
	}
	if size > maxBatchBytes {
		t.Fatalf("head is %d bytes, over %d", size, maxBatchBytes)
	}
}

func TestOneOversizedEventStillGoesOut(t *testing.T) {
	head, rest := splitAtBytes(evs(3, "/"+strings.Repeat("p", 2<<20)), maxBatchBytes)
	if len(head) != 1 || len(rest) != 2 {
		t.Fatalf("head=%d rest=%d, want 1 and 2", len(head), len(rest))
	}
}

type recorded struct {
	sizes []int
	paths []string
}

func collector(t *testing.T, fail *int32) (*httptest.Server, *recorded, *sync.Mutex) {
	t.Helper()
	rec := &recorded{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if fail != nil && atomic.LoadInt32(fail) > 0 {
			atomic.AddInt32(fail, -1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var b Batch
		if err := json.Unmarshal(body, &b); err != nil {
			t.Errorf("bad batch: %v", err)
		}
		mu.Lock()
		rec.sizes = append(rec.sizes, len(body))
		for _, e := range b.Events {
			rec.paths = append(rec.paths, e.Path)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv, rec, &mu
}

func longEvents(n int) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = Event{Direction: "out", Caller: "a", Target: "b", Method: "GET", Status: 200, Path: fmt.Sprintf("/%04d/%s", i, strings.Repeat("p", 30<<10))}
	}
	return out
}

func TestAFlushOfLongPathsGoesOutInOrderAndEveryRequestIsUnderTheGatewayLimit(t *testing.T) {
	srv, rec, mu := collector(t, nil)
	cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
	s := newSender(cfg, "go-test-sender", "test", newRing(4000), &atomicCounters{})
	s.cfg.flushBatch = 2000
	events := longEvents(60)
	for _, e := range events {
		_, _ = s.ring.push(e)
	}
	s.flush()
	mu.Lock()
	defer mu.Unlock()
	if len(rec.paths) != 60 {
		t.Fatalf("collector got %d events, want 60", len(rec.paths))
	}
	for i, p := range rec.paths {
		if p != events[i].Path {
			t.Fatalf("event %d out of order", i)
		}
	}
	if len(rec.sizes) < 2 {
		t.Fatalf("one request of %v bytes; 60 events of 30 KB must be split", rec.sizes)
	}
	for _, n := range rec.sizes {
		if n >= 1<<20 {
			t.Errorf("a request of %d bytes would be refused by the gateway's 1 MB limit", n)
		}
	}
}

func TestAFailedRequestKeepsItsEventsAndTheTailInOrderForTheNextFlush(t *testing.T) {
	fail := int32(1)
	srv, rec, mu := collector(t, &fail)
	cfg := config{enabled: true, collectorURL: srv.URL, writerAPIKey: "writer-key"}
	s := newSender(cfg, "go-test-sender", "test", newRing(4000), &atomicCounters{})
	s.cfg.flushBatch = 2000
	events := longEvents(60)
	for _, e := range events {
		_, _ = s.ring.push(e)
	}
	s.flush() // first request answered 503
	if len(s.pending) == 0 || len(s.tail) == 0 {
		t.Fatalf("pending=%d tail=%d, want both kept", len(s.pending), len(s.tail))
	}
	s.flush()
	mu.Lock()
	defer mu.Unlock()
	if len(rec.paths) != 60 {
		t.Fatalf("collector got %d events after the retry, want 60 (none lost, none repeated)", len(rec.paths))
	}
	for i, p := range rec.paths {
		if p != events[i].Path {
			t.Fatalf("event %d out of order after the retry", i)
		}
	}
}

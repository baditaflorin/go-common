package graph

import (
	"net/http"
	"strings"
	"testing"
)

func TestEscapedEventBatchesRespectEncodedByteLimit(t *testing.T) {
	srv, rec, mu := collector(t, nil)
	s := newSender(config{enabled: true, collectorURL: srv.URL, writerAPIKey: "test", flushBatch: 2000}, "svc", "test", newRing(2000), &atomicCounters{})
	// Raw lengths fit the packing estimate, but '<' and '&' encode to six
	// bytes each. Quotes, controls and invalid UTF-8 exercise other escapes.
	for i := 0; i < 100; i++ {
		s.ring.push(Event{Direction: "out", Caller: "svc", Target: "target", Path: strings.Repeat("<&\"\x01\xff", 1000), Method: http.MethodGet, Status: 200})
	}
	s.flush()
	mu.Lock()
	defer mu.Unlock()
	if len(rec.paths) != 100 {
		t.Fatalf("delivered %d events, want 100", len(rec.paths))
	}
	if len(rec.sizes) < 2 {
		t.Fatal("escaped payload was not split")
	}
	for _, n := range rec.sizes {
		if n > maxBatchBytes {
			t.Fatalf("POST size %d exceeds %d", n, maxBatchBytes)
		}
	}
	if got := s.counters.snapshot(); got.BatchesRejected != 0 || got.BatchesFailed != 0 {
		t.Fatalf("split batches counted as lost: %+v", got)
	}
}

func TestOneUnencodableSizeEventDoesNotBlockLaterEvents(t *testing.T) {
	srv, rec, mu := collector(t, nil)
	s := newSender(config{enabled: true, collectorURL: srv.URL, writerAPIKey: "test", flushBatch: 2000}, "svc", "test", newRing(3), &atomicCounters{})
	for _, path := range []string{strings.Repeat("<", maxBatchBytes), "/second", "/third"} {
		s.ring.push(Event{Direction: "out", Caller: "svc", Target: "target", Path: path, Method: http.MethodGet, Status: 200})
	}
	s.flush()
	mu.Lock()
	defer mu.Unlock()
	if len(rec.paths) != 2 || rec.paths[0] != "/second" || rec.paths[1] != "/third" {
		t.Fatalf("delivered paths = %v", rec.paths)
	}
	if got := s.counters.snapshot(); got.BatchesRejected != 1 || got.BatchesFailed != 1 {
		t.Fatalf("oversize counters = %+v", got)
	}
	if s.ring.len() != 0 || len(s.pending) != 0 || len(s.tail) != 0 {
		t.Fatal("oversized event remains queued")
	}
}

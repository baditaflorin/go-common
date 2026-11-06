package safehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLegacyTraceEmissionBoundsSlowCollectorAndStops(t *testing.T) {
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	emitter := newLegacyTraceEmitter(2)
	tr := &extrasTransport{traceURL: server.URL, traceEmitter: emitter}
	for range 2 {
		tr.queueTrace(traceFields{Host: "example.com"})
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("collector request did not start")
		}
	}
	for range 1000 {
		tr.queueTrace(traceFields{Host: "example.com"})
	}
	if got := emitter.snapshot(); got.InFlight != 2 || got.Dropped != 1000 {
		t.Fatalf("sender stats=%+v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := emitter.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := emitter.snapshot().InFlight; got != 0 {
		t.Fatalf("shutdown retained %d emissions", got)
	}
	tr.queueTrace(traceFields{Host: "example.com"})
	if got := emitter.snapshot().Dropped; got != 1001 {
		t.Fatalf("post-shutdown dropped=%d", got)
	}
	if err := emitter.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyTraceShutdownHonorsDeadlineWithoutWaiterGoroutine(t *testing.T) {
	emitter := newLegacyTraceEmitter(1)
	if _, ok := emitter.acquire(); !ok {
		t.Fatal("first admission refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := emitter.shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown err=%v", err)
	}
	emitter.release()
	if err := emitter.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkLegacyTraceDrop(b *testing.B) {
	emitter := newLegacyTraceEmitter(1)
	_, _ = emitter.acquire()
	tr := &extrasTransport{traceEmitter: emitter}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tr.queueTrace(traceFields{Host: "example.com"})
	}
	b.StopTimer()
	emitter.release()
	_ = emitter.shutdown(context.Background())
}

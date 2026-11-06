package safehttp

import (
	"context"
	"sync"
)

const legacyTraceConcurrency = 32

var defaultLegacyTraceEmitter = newLegacyTraceEmitter(legacyTraceConcurrency)

// TraceEmissionStats describes the legacy WithTraceCollector sender. OpenTelemetry
// exporters and graph batching have their own lifecycle and counters.
type TraceEmissionStats struct {
	InFlight int
	Dropped  uint64
}

// TraceEmissionSnapshot returns current legacy trace emission resource counters.
func TraceEmissionSnapshot() TraceEmissionStats { return defaultLegacyTraceEmitter.snapshot() }

// ShutdownTraceEmission stops accepting legacy trace emissions, cancels active
// collector calls, and waits for their goroutines to exit within ctx. It is
// idempotent and process-wide; use it during final process shutdown after callers
// stop sending requests. It does not shut down OpenTelemetry or graph exporters.
func ShutdownTraceEmission(ctx context.Context) error { return defaultLegacyTraceEmitter.shutdown(ctx) }

type legacyTraceEmitter struct {
	mu         sync.Mutex
	limit      int
	inFlight   int
	dropped    uint64
	stopped    bool
	doneClosed bool
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
}

func newLegacyTraceEmitter(limit int) *legacyTraceEmitter {
	ctx, cancel := context.WithCancel(context.Background())
	return &legacyTraceEmitter{limit: limit, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (e *legacyTraceEmitter) acquire() (context.Context, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.inFlight >= e.limit {
		e.dropped++
		return nil, false
	}
	e.inFlight++
	return e.ctx, true
}

func (e *legacyTraceEmitter) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inFlight--
	e.closeDoneLocked()
}

func (e *legacyTraceEmitter) closeDoneLocked() {
	if e.stopped && e.inFlight == 0 && !e.doneClosed {
		e.doneClosed = true
		close(e.done)
	}
}

func (e *legacyTraceEmitter) snapshot() TraceEmissionStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return TraceEmissionStats{InFlight: e.inFlight, Dropped: e.dropped}
}

func (e *legacyTraceEmitter) shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = true
	e.cancel()
	e.closeDoneLocked()
	e.mu.Unlock()
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *extrasTransport) queueTrace(fields traceFields) {
	emitter := t.traceEmitter
	if emitter == nil {
		emitter = defaultLegacyTraceEmitter
	}
	ctx, ok := emitter.acquire()
	if !ok {
		return
	}
	go func() { defer emitter.release(); t.emitTrace(ctx, fields) }()
}

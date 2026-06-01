package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushRecorder struct {
	http.ResponseWriter
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++ }

// The metrics/logging wrappedWriter must support streaming so handlers can
// chunk responses (SSE, tail -f, long-poll). Regression guard for the
// 2026-06-01 "streaming unsupported" bug where w.(http.Flusher) failed on the
// wrapper.
func TestWrappedWriterFlushAndUnwrap(t *testing.T) {
	inner := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	w := &wrappedWriter{ResponseWriter: inner, status: http.StatusOK}

	fl, ok := interface{}(w).(http.Flusher)
	if !ok {
		t.Fatal("wrappedWriter must implement http.Flusher")
	}
	fl.Flush()
	if inner.flushes != 1 {
		t.Fatalf("Flush not forwarded to inner: got %d", inner.flushes)
	}

	if w.Unwrap() != inner {
		t.Fatal("Unwrap must return the wrapped ResponseWriter")
	}
	// http.ResponseController must reach the flusher through w.
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush: %v", err)
	}
	if inner.flushes != 2 {
		t.Fatalf("ResponseController.Flush not forwarded: got %d", inner.flushes)
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/config"
)

// BenchmarkServerRequest is the per-request overhead of the whole fleet stack
// (telemetry, request id, metrics, body limit, promx, graph) around a handler
// that does nothing. It is the number that every service pays on every request,
// so scripts/bench-gate.sh watches its allocations.
//
// The access log is skipped for the benchmarked path (LOG_SKIP_PATHS, read once
// per process): the logger writes JSON to stdout and would bury the results.
// middleware's own benchmark measures the logger with its output discarded.
func BenchmarkServerRequest(b *testing.B) {
	b.Setenv("LOG_SKIP_PATHS", "/bench")

	srv := New(&config.Config{AppName: "go_bench", Version: "0.0.0", Port: "0"})
	srv.Mux.HandleFunc("/bench", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/bench", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

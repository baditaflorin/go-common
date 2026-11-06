package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/middleware"
)

// BenchmarkServerRequest is the per-request overhead of the whole fleet stack
// (telemetry, request id, metrics, body limit, promx, graph) around a handler
// that does nothing. It is the number that every service pays on every request,
// so scripts/bench-gate.sh watches its allocations.
//
// This historical baseline measures an incoming request ID and a skipped log.
// GeneratedID and Logged below cover the additional work on ordinary requests.
func BenchmarkServerRequest(b *testing.B) {
	h := serverBenchmarkHandler(b)
	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	// Previously only the first iteration generated an ID: the middleware
	// mutates this header. Make the subsequent incoming-ID branch explicit.
	req.Header.Set(header.RequestID, "benchmark-incoming-id")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func serverBenchmarkHandler(b *testing.B) http.Handler {
	b.Helper()
	// Every benchmark uses the same skip list: Logging reads it once per
	// process. Changing it between cohorts would make results order dependent.
	b.Setenv("LOG_SKIP_PATHS", "/bench")
	b.Setenv("LOG_FORMAT", "text")
	middleware.SetLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.Cleanup(func() { middleware.SetLogger(slog.New(slog.NewJSONHandler(os.Stdout, nil))) })
	srv := New(&config.Config{AppName: "go_bench", Version: "0.0.0", Port: "0"})
	srv.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return srv.Handler()
}

func benchmarkServerGeneratedID(b *testing.B, path string) {
	b.Helper()
	h := serverBenchmarkHandler(b)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Header.Del(header.RequestID)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkServerRequestGeneratedID(b *testing.B) {
	benchmarkServerGeneratedID(b, "/bench")
}

// Discard only the output: the JSON access record is still encoded.
func BenchmarkServerRequestLogged(b *testing.B) {
	benchmarkServerGeneratedID(b, "/api/v1/items")
}

func BenchmarkServerRequestParallel(b *testing.B) {
	h := serverBenchmarkHandler(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
		for pb.Next() {
			req.Header.Del(header.RequestID)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

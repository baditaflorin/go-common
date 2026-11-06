package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/metrics"
)

func benchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// quietLogs sends the access log to io.Discard. The logger still builds and
// encodes every record, so its CPU and allocation cost stay in the measurement;
// only the write to stdout is dropped.
func quietLogs(b *testing.B) {
	b.Helper()
	b.Setenv("LOG_SKIP_PATHS", "/bench")
	resetSkipPaths()
	b.Cleanup(resetSkipPaths)
	prev := defaultLogger
	defaultLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.Cleanup(func() { defaultLogger = prev })
}

func BenchmarkRequestID(b *testing.B) {
	h := RequestID(benchHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// The historical benchmark reused the generated header after its first
	// iteration. Seed it explicitly to keep that incoming-ID baseline honest.
	req.Header.Set(header.RequestID, "benchmark-incoming-id")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// The three middlewares every service gets, in the order server.New applies them,
// with an incoming ID. GeneratedID below covers requests without one.
func BenchmarkChainRequestIDLoggingMetrics(b *testing.B) {
	quietLogs(b)
	h := Chain(benchHandler(), RequestID, Logging, Metrics(metrics.New()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	req.Header.Set(header.RequestID, "benchmark-incoming-id")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// Generated IDs must be measured on every iteration: RequestID stamps the
// inbound header, so reusing a request without clearing it measures the other
// branch after the first iteration.
func BenchmarkRequestIDGenerated(b *testing.B) {
	h := RequestID(benchHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Header.Del(header.RequestID)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkChainGeneratedID(b *testing.B) {
	quietLogs(b)
	h := Chain(benchHandler(), RequestID, Logging, Metrics(metrics.New()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Header.Del(header.RequestID)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkChainGeneratedIDParallel(b *testing.B) {
	quietLogs(b)
	h := Chain(benchHandler(), RequestID, Logging, Metrics(metrics.New()))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Each worker owns its request and mutable header map.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
		for pb.Next() {
			req.Header.Del(header.RequestID)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

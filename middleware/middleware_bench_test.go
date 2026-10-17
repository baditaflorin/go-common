package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
	prev := defaultLogger
	defaultLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.Cleanup(func() { defaultLogger = prev })
}

func BenchmarkRequestID(b *testing.B) {
	h := RequestID(benchHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// The three middlewares every service gets, in the order server.New applies them.
func BenchmarkChainRequestIDLoggingMetrics(b *testing.B) {
	quietLogs(b)
	h := Chain(benchHandler(), RequestID, Logging, Metrics(metrics.New()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/items", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

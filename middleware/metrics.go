package middleware

import (
	"net/http"

	"github.com/baditaflorin/go-common/metrics"
)

// MetricsMiddleware records request stats
func Metrics(stats *metrics.Stats) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := &wrappedWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)
			stats.Record(ww.status)
		})
	}
}

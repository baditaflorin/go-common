package middleware

import (
	"log/slog"
	"net/http"
	"os"
	"time"
)

// Logging logs request details using slog
func Logging(next http.Handler) http.Handler {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap ResponseWriter to capture status code
		ww := &wrappedWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(ww, r)

		duration := time.Since(start)
		reqID := GetRequestID(r.Context())

		logger.Info("request_completed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.status),
			slog.String("duration", duration.String()),
			slog.String("ip", r.RemoteAddr),
			slog.String("request_id", reqID),
		)
	})
}

type wrappedWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrappedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

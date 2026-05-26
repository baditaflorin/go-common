package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// loggerKey is the context key used to store the per-request slog.Logger.
type loggerKey struct{}

// LoggerFromContext retrieves the per-request logger injected by the
// Logging middleware. Falls back to the default slog logger so callers
// don't need to handle the nil case.
//
//	func MyHandler(w http.ResponseWriter, r *http.Request) {
//	    log := middleware.LoggerFromContext(r.Context())
//	    log.Info("processing", "user", userID)
//	}
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithLogger injects logger into ctx. Used internally by the Logging
// middleware and available for testing (inject a no-op logger).
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// defaultLogger is the process-wide JSON logger shared across requests.
// Constructed once at package init to avoid repeated allocations.
var defaultLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

// skipPaths holds the parsed LOG_SKIP_PATHS prefix list.
// Initialised lazily (once) so tests can set the env var before first use.
var (
	skipPathsOnce sync.Once
	skipPaths     []string
)

// loadSkipPaths parses LOG_SKIP_PATHS on first call and caches the result.
// Each entry is a non-empty path prefix; whitespace around entries is trimmed.
func loadSkipPaths() []string {
	skipPathsOnce.Do(func() {
		raw := os.Getenv("LOG_SKIP_PATHS")
		if raw == "" {
			return
		}
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				skipPaths = append(skipPaths, p)
			}
		}
	})
	return skipPaths
}

// isSkipped reports whether path starts with any of the configured skip prefixes.
func isSkipped(path string) bool {
	for _, prefix := range loadSkipPaths() {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Logging logs request details using slog and injects a per-request
// child logger (enriched with request_id, method, path) into the context
// so downstream handlers can call middleware.LoggerFromContext(r.Context()).
//
// When LOG_SKIP_PATHS is set (comma-separated path prefixes, e.g.
// "/health,/metrics"), the request_completed entry for matching paths is
// emitted at DEBUG level instead of INFO. This suppresses health-check
// noise without losing the events entirely. When LOG_SKIP_PATHS is unset
// or empty, all requests are logged at INFO (fully backward-compatible).
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := GetRequestID(r.Context())

		// Build a per-request child logger with shared request fields.
		reqLogger := defaultLogger.With(
			slog.String("request_id", reqID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("ip", r.RemoteAddr),
		)

		// Inject the child logger into the context before calling next.
		ctx := WithLogger(r.Context(), reqLogger)
		r = r.WithContext(ctx)

		// Wrap ResponseWriter to capture status code and bytes written.
		ww := &wrappedWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(ww, r)

		duration := time.Since(start)
		attrs := []any{
			slog.Int("status", ww.status),
			slog.String("duration", duration.String()),
			slog.Int64("bytes", ww.bytes),
		}

		if isSkipped(r.URL.Path) {
			reqLogger.Debug("request_completed", attrs...)
		} else {
			reqLogger.Info("request_completed", attrs...)
		}
	})
}

type wrappedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *wrappedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write tracks the response byte count so middleware that cares about
// response size (e.g. Prometheus http_response_size_bytes) doesn't need
// to wrap the writer a second time.
func (w *wrappedWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

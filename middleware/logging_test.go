package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// resetSkipPaths wipes the package-level once + slice so each test starts
// fresh. Must be called before mutating LOG_SKIP_PATHS in tests.
func resetSkipPaths() {
	skipPathsOnce = sync.Once{}
	skipPaths = nil
}

// captureLogger returns a slog.Logger that writes JSON lines into buf and
// uses the given minimum level so DEBUG entries are not silently discarded.
func captureLogger(buf *bytes.Buffer, minLevel slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: minLevel}))
}

// parseLevel extracts the "level" field from a single JSON log line.
func parseLevel(t *testing.T, line string) string {
	t.Helper()
	line = strings.TrimSpace(line)
	if line == "" {
		t.Fatal("no log line captured")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("failed to parse log JSON %q: %v", line, err)
	}
	lvl, _ := m["level"].(string)
	return strings.ToUpper(lvl)
}

// makeHandler returns a Logging-wrapped no-op handler that writes into buf,
// using a custom logger rather than the package-level defaultLogger.
// We swap defaultLogger for the test duration.
func makeHandler(buf *bytes.Buffer, minLevel slog.Level) http.Handler {
	saved := defaultLogger
	defaultLogger = captureLogger(buf, minLevel)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = saved // keep reference alive
		Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, r)
		defaultLogger = saved
	})
}

// loggingMiddlewareLevel fires a single request through the Logging middleware
// using a captured logger and returns the slog level string of the
// request_completed entry.
func loggingMiddlewareLevel(t *testing.T, path string) string {
	t.Helper()

	var buf bytes.Buffer
	// Replace the package-level defaultLogger for this test.
	saved := defaultLogger
	defaultLogger = captureLogger(&buf, slog.LevelDebug)
	t.Cleanup(func() { defaultLogger = saved })

	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	Logging(noop).ServeHTTP(rr, req)

	// Find the request_completed line.
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "request_completed") {
			return parseLevel(t, line)
		}
	}
	t.Fatalf("no request_completed entry in log output:\n%s", buf.String())
	return ""
}

// TestLogging_SkipPath verifies that a path included in LOG_SKIP_PATHS is
// logged at DEBUG instead of INFO.
func TestLogging_SkipPath(t *testing.T) {
	t.Setenv("LOG_SKIP_PATHS", "/health,/metrics")
	resetSkipPaths()
	t.Cleanup(resetSkipPaths)

	if got := loggingMiddlewareLevel(t, "/health"); got != "DEBUG" {
		t.Errorf("expected DEBUG for /health, got %s", got)
	}
	if got := loggingMiddlewareLevel(t, "/metrics"); got != "DEBUG" {
		t.Errorf("expected DEBUG for /metrics, got %s", got)
	}
	// Sub-path of a skip prefix should also be demoted.
	if got := loggingMiddlewareLevel(t, "/health/live"); got != "DEBUG" {
		t.Errorf("expected DEBUG for /health/live, got %s", got)
	}
}

// TestLogging_NonSkipPath verifies that a path NOT in LOG_SKIP_PATHS is
// logged at INFO.
func TestLogging_NonSkipPath(t *testing.T) {
	t.Setenv("LOG_SKIP_PATHS", "/health,/metrics")
	resetSkipPaths()
	t.Cleanup(resetSkipPaths)

	if got := loggingMiddlewareLevel(t, "/api/users"); got != "INFO" {
		t.Errorf("expected INFO for /api/users, got %s", got)
	}
}

// TestLogging_NoSkipPaths verifies that when LOG_SKIP_PATHS is unset,
// every path is logged at INFO (backward-compatible behaviour).
func TestLogging_NoSkipPaths(t *testing.T) {
	t.Setenv("LOG_SKIP_PATHS", "")
	resetSkipPaths()
	t.Cleanup(resetSkipPaths)

	for _, path := range []string{"/health", "/metrics", "/api/orders"} {
		if got := loggingMiddlewareLevel(t, path); got != "INFO" {
			t.Errorf("expected INFO for %s when LOG_SKIP_PATHS unset, got %s", path, got)
		}
	}
}

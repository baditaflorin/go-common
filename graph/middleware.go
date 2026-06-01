package graph

import (
	"net/http"
	"strings"
	"time"
)

// Middleware records one inbound Event per served request. Mounted as
// the first middleware in server.New so it sees the real status code
// (subsequent middlewares like Logging and Metrics also run).
//
// Health/version/metrics paths are excluded to avoid drowning the
// collector in load-balancer probe traffic.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r.URL.Path) || !Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		latency := time.Since(start).Milliseconds()

		caller := callerFromUA(r.Header.Get("User-Agent"))
		if caller == "" {
			// Gateway may forward an explicit caller header for
			// internal-mesh hops where the UA was rewritten by a proxy.
			if h := r.Header.Get("X-Fleet-Caller"); h != "" {
				caller = strings.TrimSpace(h)
			}
		}
		if caller == "" {
			caller = "external:client"
		}

		Record(Event{
			Direction: "in",
			Caller:    caller,
			// Target filled in by Record from package identity.
			Path:      templatisePath(r.URL.Path),
			Method:    r.Method,
			Status:    sw.status,
			LatencyMs: latency,
		})
	})
}

// statusWriter captures the status code so the middleware can report it.
// Faithful subset of httpsnoop / chi MiddlewareWriter; kept inline so
// graph stays dependency-free.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
		// status remains 200 (the default); matches http.ResponseWriter contract
	}
	return s.ResponseWriter.Write(b)
}

// Flush + Unwrap let streaming handlers (SSE, tail -f, long-poll) work through
// this wrapper. Without them w.(http.Flusher) fails and streaming bails with
// "streaming unsupported".
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func isProbe(path string) bool {
	switch path {
	case "/health", "/version", "/metrics", "/_gw_health", "/capabilities":
		return true
	}
	return false
}

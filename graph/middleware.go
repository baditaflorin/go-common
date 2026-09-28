package graph

import (
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/baditaflorin/go-common/internal/graphidentity"
)

// Middleware records one inbound Event per authenticated request. server.New
// mounts it inside authentication middleware so it can read a verified
// principal from context. Requests that have no verified service identity are
// recorded with caller "unknown".
//
// Health/version/metrics paths are excluded to avoid drowning the
// collector in load-balancer probe traffic.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The collector can observe its own public request surface, but it
		// must never observe the authenticated batch that its sender posts
		// back to /events. The outbound transport already bypasses collector
		// requests; without this matching inbound guard the collector would
		// generate a fresh inbound event for every successful flush.
		if isProbe(r.URL.Path) || isCollectorIngest(r) || !Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		latency := time.Since(start).Milliseconds()

		// Request headers are claims, not proof of service identity. The
		// keystore auth middleware marks a caller in context only after a
		// trusted gateway or direct keystore verification accepts it.
		caller := graphidentity.VerifiedPrincipal(r.Context())
		if caller == "" {
			// Custom servers may not use go-common/server's auth middleware.
			// Trust the gateway identity header only from an explicitly listed
			// TCP peer.
			caller = trustedGatewayCaller(r, ensureInit().cfg.trustedCallerIPs)
		}
		if caller == "" {
			caller = "unknown"
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

func trustedGatewayCaller(r *http.Request, trustedIPs []netip.Addr) string {
	if len(trustedIPs) == 0 {
		return ""
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return ""
	}
	ip = ip.Unmap()
	for _, trusted := range trustedIPs {
		if ip == trusted {
			return graphidentity.NormalizeServiceCallerID(r.Header.Get("X-Auth-User"))
		}
	}
	return ""
}

// isCollectorIngest identifies only the graph sender's event-batch request.
// A normal reader request to the collector remains observable, which lets the
// collector report its own use without creating an ingest feedback loop.
func isCollectorIngest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/events" && isCollectorURL(r)
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

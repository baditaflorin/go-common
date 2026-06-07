package server

import (
	"encoding/json"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/obs"
	"github.com/baditaflorin/go-common/promx"
	"github.com/baditaflorin/go-common/reqstats"
	"github.com/baditaflorin/go-common/safehttp"
	"log"
	"net/http"
	"os"
	"time"
)

// 4 MiB

type Option func(*Server)

// WithoutRequestStats disables the default per-request reqstats middleware
// (Server-Timing + X-Request-Stats headers). Enabled by default for every
// service; opt out only if the extra headers or the cheap getrusage sample
// are genuinely unwanted.
func WithoutRequestStats() Option {
	return func(s *Server) { s.noRequestStats = true }
}

// New creates a new Server with optional configuration
func New(cfg *config.Config, opts ...Option) *Server {
	// Initialise the fleet-graph identity for this process. Safe to
	// call multiple times; subsequent calls only update identity.
	// All outbound (safehttp) + inbound (graph.Middleware below) events
	// are tagged with cfg.AppName from here on.
	graph.Init(cfg.AppName, cfg.Version)

	mux := http.NewServeMux()
	stats := metrics.New()

	// Auto-wire promx BEFORE options run so WithKeystoreAuth (and any
	// future option that needs access to the prom collectors) sees a
	// non-nil PromAuthCollectors / PromHTTPCollectors. AutoWire is
	// idempotent across repeated server.New calls in tests — same
	// process, same collectors.
	egressColl, httpColl, authColl := promx.AutoWire(cfg.AppName, cfg.Version)
	safehttp.SetDefaultObserver(egressColl)

	// Auto-wire the fleet fetch-cache delegate when FLEET_FETCH_CACHE_URL
	// is set. From here on every safehttp client constructed in this
	// process transparently routes its eligible outbound GETs through the
	// fleet fetch cache (server-side singleflight + caching), with zero
	// per-service code changes. Clients that need real direct egress
	// (WithoutProxy SSRF probers, or explicit WithoutFetchCache) are not
	// affected — see safehttp.NewClient delegate resolution. A cache
	// outage falls through to direct egress, so this is fail-open.
	if cacheURL := os.Getenv(fleetfetch.EnvCacheURL); cacheURL != "" {
		ff := fleetfetch.NewClient() // reads FLEET_FETCH_CACHE_URL + API key from env
		safehttp.SetDefaultFetchDelegate(fetchCacheDelegate{ff})
	}
	// AutoWire has already installed process-wide observers for
	// response.Envelope, degraded.Sink, fleetfetch.Client,
	// circuitbreaker, workpool, backoffcoord, and the safehttp
	// backoff-consult path. apikey admin observer is per-client and
	// must be attached by callers that construct an apikey.Client
	// directly (or by future server-side helpers that own one).

	srv := &Server{
		Config:             cfg,
		Mux:                mux,
		Middlewares:        []middleware.Middleware{},
		Stats:              stats,
		PromHTTPCollectors: httpColl,
		PromAuthCollectors: authColl,
		maxBodyBytes:       DefaultMaxBodyBytes,
		drainTimeout:       DefaultDrainTimeout,
	}

	// Apply options after promx collectors are wired so option handlers
	// (e.g. WithKeystoreAuth) can attach themselves to the collectors.
	for _, opt := range opts {
		opt(srv)
	}

	// Register /health endpoint. Two shapes:
	//   - no deps registered → flat {"status":"healthy", ...} (legacy)
	//   - deps registered    → {"status":"healthy|degraded", ...,
	//                            "dependencies":[…depcheck.Status]}
	// HTTP status code is always 200 — degraded is a soft signal, not a
	// liveness failure.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		payload := map[string]interface{}{
			"status":  "healthy",
			"service": cfg.AppName,
			"version": cfg.Version,
		}
		if srv.Deps != nil {
			statuses := srv.Deps.Snapshot(r.Context())
			payload["dependencies"] = statuses
			if !depcheck.AllOK(statuses) {
				payload["status"] = "degraded"
			}
		}
		_ = json.NewEncoder(w).Encode(payload)
	})

	// Register /readyz endpoint — the readiness probe consulted by
	// load balancers. Always installed (independent of
	// WithGracefulDrain) so the route is uniform across the fleet; it
	// returns 200 in normal operation and flips to 503 only after
	// BeginDrain has been called on a drain-enabled server.
	mountReadyz(srv)

	// Register /version endpoint
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cfg.Version))
	})

	// /metrics is NOT mounted here on srv.Mux because services that
	// already registered their own /metrics handler (e.g. with
	// promhttp.Handler()) would panic with "multiple registrations"
	// on their next redeploy. Instead, Start() wraps the final handler
	// with metricsAwareHandler — if no /metrics route is registered by
	// the time Start runs, the wrapper serves promx.Handler(). User
	// registrations always win.
	srv.promMetricsHandler = promx.Handler()
	mux.HandleFunc("/metrics/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stats.Snapshot())
	})

	// Publish the schema version (defaulting if unset) before mounting
	// /capabilities + /schema so both endpoints serialize the same value
	// and response.Envelope picks up the service identity.
	publishSchemaVersion(srv)

	// Register /capabilities endpoint — fleet-wide flag discovery.
	// See server/capabilities.go for the rationale.
	mountCapabilities(srv)

	// Register /schema endpoint — fleet-wide breaking-change signal.
	// See server/schema.go for the rationale.
	mountSchema(srv)

	// Add Default Middlewares (executed in slice order — [0] is outermost)
	// 0. reqstats (outermost: measures total wall-time incl. all middleware,
	//    emits Server-Timing + X-Request-Stats on every response)
	// 1. Graph observer (sees final status + latency, records inbound Event)
	// 2. RequestID
	// 3. Logging (injects per-request slog logger into context)
	// 4. BodyLimit (rejects oversized bodies before auth / handler)
	// 5. Metrics (Record Status) — JSON snapshot surface
	// 6. PromHTTP (Record Status) — Prometheus surface
	defaultMWs := []middleware.Middleware{}
	if !srv.noRequestStats {
		defaultMWs = append(defaultMWs, reqstats.Middleware(srv.Config.AppName, srv.Config.Version))
	}
	defaultMWs = append(defaultMWs,
		graph.Middleware,
		middleware.RequestID,
		middleware.Logging,
	)
	if srv.maxBodyBytes > 0 {
		defaultMWs = append(defaultMWs, bodyLimitMiddleware(srv.maxBodyBytes))
	}
	defaultMWs = append(defaultMWs,
		middleware.Metrics(stats),
		httpColl.Middleware(),
	)
	srv.Middlewares = append(defaultMWs, srv.Middlewares...)

	// Auto-wire the localhost-only debug server (net/http/pprof +
	// /metrics mirror), gated by the DEBUG_ADDR / OBS_DISABLE env knobs
	// (default ON, bound to 127.0.0.1:6060). This gives every fleet
	// service pprof for diagnosing RSS creep / goroutine leaks before an
	// OOM, with zero per-service code — adoption is automatic on the
	// next go-common bump. pprof is loopback-only by design; it must
	// never be reachable from the public gateway. The runtime + process
	// collectors backing its /metrics are already registered by
	// promx.Init (called via AutoWire above), so this adds no new
	// metrics, only a safe local pprof surface. A bind failure is
	// logged, not fatal — a debug aid must never stop a service from
	// booting. The stop func is invoked from Start()'s shutdown paths.
	if stop, err := obs.Init(); err != nil {
		log.Printf("server: debug server (pprof) not started: %v", err)
	} else {
		srv.debugStop = stop
	}

	return srv
}

// resolveTimeout picks the effective value for one http.Server timeout.
// Precedence, highest first:
//
//  1. an explicit option override (WithWriteTimeout / WithReadTimeout /
//     WithServerTimeouts set the field > 0) — a service's deliberate,
//     code-reviewed intent always wins;
//  2. the SERVER_*_TIMEOUT_SECONDS env knob (> 0) — lets ops raise a
//     render-heavy service's deadline via compose without a code change.
//     This is the path bare server.Run services (which don't pass an
//     Option) use to lift the 30 s write cap that 502s cold JS renders;
//  3. the compiled-in Default* — unchanged behaviour for every service
//     that sets neither.
//
// A non-positive env value (unset, empty, "0", negative, or garbage) is
// ignored and falls through to the default — env.Int already logs and
// returns the 0 default on a parse error.
func resolveTimeout(override time.Duration, envSecondsVar string, def time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if n := env.Int(envSecondsVar, 0); n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// HealthBody is the JSON response shape for /health.
type healthBody struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// HealthHandler returns an http.Handler for GET /health.
// Response: {"status":"ok","service":"<id>","version":"<ver>"}
func HealthHandler(serviceID, version string) http.Handler {
	b, _ := json.Marshal(healthBody{Status: "ok", Service: serviceID, Version: version})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}

// VersionHandler returns an http.Handler for GET /version.
// Response: {"version":"<ver>"}
func VersionHandler(version string) http.Handler {
	b, _ := json.Marshal(map[string]string{"version": version})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}

// bodyLimitMiddleware wraps each handler so that request bodies
// exceeding maxBytes are rejected with HTTP 413 before being read.
// This prevents memory exhaustion from malicious oversized payloads.
func bodyLimitMiddleware(maxBytes int64) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

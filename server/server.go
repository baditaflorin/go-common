package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/client"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/promx"
	"github.com/baditaflorin/go-common/safehttp"
)

type Server struct {
	Config       *config.Config
	Mux          *http.ServeMux
	Middlewares  []middleware.Middleware
	Stats        *metrics.Stats
	Deps         *depcheck.Registry
	Capabilities []client.Capability
	// SchemaVersion is the integer that monotonically increases on every
	// breaking JSON-shape change. Set via WithSchemaVersion(N); defaults
	// to DefaultSchemaVersion. Exposed at GET /schema, embedded in
	// GET /capabilities, and stamped on response.Envelope outputs.
	SchemaVersion int

	// PromHTTPCollectors is the inbound HTTP collector set, auto-
	// registered into the shared promx registry. Exposed so callers can
	// inspect or, in rare cases, supply a custom RouteFunc (e.g. when
	// wrapping a router that exposes a templated-path API).
	PromHTTPCollectors *promx.HTTPCollectors
	// PromAuthCollectors is the keystore-auth collector set. Passed to
	// WithKeystoreAuth automatically so apikey_auth_total and friends
	// populate without per-service wiring.
	PromAuthCollectors *promx.AuthCollectors
}

type Option func(*Server)

// WithMiddleware adds middlewares to the server
func WithMiddleware(mws ...middleware.Middleware) Option {
	return func(s *Server) {
		s.Middlewares = append(s.Middlewares, mws...)
	}
}

// WithKeystoreAuth mounts the canonical fleet auth middleware
// (middleware.TokenAuthKeystore) wired up to the keystore via
// apikey.New() + apikey.Cache. Suitable for every 0exec service —
// gateway X-Auth-User is trusted on the hot path, the keystore is
// only called when the gateway is bypassed.
//
// localTokens are pre-trusted without hitting the keystore — e.g. the
// gateway's static fallback key, or "default_token" for demos.
// Pass none for keystore-only.
//
//	srv := server.New(cfg, server.WithKeystoreAuth("default_token"))
//
// Reads APIKEY_SERVICE_URL + APIKEY_SERVICE_ADMIN_TOKEN from env (with
// sane defaults). Failures are deferred to first /verify call — the
// service starts even if the keystore is unreachable, and per-request
// behavior falls through to the local-token fast path or fails closed
// with 503. /health, /version, /_gw_health are always exempt.
func WithKeystoreAuth(localTokens ...string) Option {
	return func(s *Server) {
		ks := apikey.NewCache(apikey.New())
		// Wire promx observer on both halves of the auth path so the
		// fleet auth dashboard populates without per-service wiring.
		// s.PromAuthCollectors is set by New() before options run, so
		// this assignment is always safe.
		if s.PromAuthCollectors != nil {
			ks.Observer = s.PromAuthCollectors
		}
		s.Middlewares = append(s.Middlewares, middleware.TokenAuthKeystore(middleware.KeystoreOpts{
			Verifier:    ks,
			LocalTokens: localTokens,
			Observer:    s.PromAuthCollectors,
		}))
	}
}

// WithDependencies attaches a dep registry whose probes are run on every
// /health request. Health JSON gains a "dependencies":[…] array and the
// top-level "status" flips to "degraded" if any probe fails (HTTP stays
// 200 so a soft-dep blip doesn't tear the container down). See
// depcheck package doc for the JSON contract.
//
// Pass exactly one registry — calling WithDependencies twice replaces
// the previous registry.
func WithDependencies(r *depcheck.Registry) Option {
	return func(s *Server) {
		s.Deps = r
	}
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

	srv := &Server{
		Config:             cfg,
		Mux:                mux,
		Middlewares:        []middleware.Middleware{},
		Stats:              stats,
		PromHTTPCollectors: httpColl,
		PromAuthCollectors: authColl,
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

	// Register /version endpoint
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cfg.Version))
	})

	// Prometheus /metrics is the canonical scrape endpoint (Hub +
	// fleet convention). The legacy JSON-Snapshot surface moves to
	// /metrics/json so any consumer that depended on it can migrate
	// without a flag day.
	mux.Handle("/metrics", promx.Handler())
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
	// 1. Graph observer (outermost: sees final status + latency including
	//    other middleware overhead, records inbound Event)
	// 2. RequestID (Start)
	// 3. Logging
	// 4. Metrics (Record Status) — JSON snapshot surface
	// 5. PromHTTP (Record Status) — Prometheus surface; sits alongside #4
	//    so both surfaces populate from the same request hot path.
	srv.Middlewares = append([]middleware.Middleware{
		graph.Middleware,
		middleware.RequestID,
		middleware.Logging,
		middleware.Metrics(stats),
		httpColl.Middleware(),
	}, srv.Middlewares...)

	return srv
}

func (s *Server) Start() {
	addr := ":" + s.Config.Port
	fmt.Printf("Starting %s v%s on %s (Middleware+Metrics Enabled)\n", s.Config.AppName, s.Config.Version, addr)

	// Wrap the mux with middlewares
	finalHandler := middleware.Chain(s.Mux, s.Middlewares...)

	srv := &http.Server{
		Addr:         addr,
		Handler:      finalHandler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())
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

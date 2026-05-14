package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
)

type Server struct {
	Config      *config.Config
	Mux         *http.ServeMux
	Middlewares []middleware.Middleware
	Stats       *metrics.Stats
	Deps        *depcheck.Registry
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
		s.Middlewares = append(s.Middlewares, middleware.TokenAuthKeystore(middleware.KeystoreOpts{
			Verifier:    ks,
			LocalTokens: localTokens,
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
	mux := http.NewServeMux()
	stats := metrics.New()

	srv := &Server{
		Config:      cfg,
		Mux:         mux,
		Middlewares: []middleware.Middleware{},
		Stats:       stats,
	}

	// Apply options first so dependency registry (and any other
	// option-driven state) is visible to the /health handler we mount
	// below.
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

	// Register /metrics endpoint (Phase 3)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(stats.Snapshot())
	})

	// Add Default Middlewares
	// 1. RequestID (Start)
	// 2. Logging
	// 3. Metrics (Record Status)
	srv.Middlewares = append([]middleware.Middleware{
		middleware.RequestID,
		middleware.Logging,
		middleware.Metrics(stats),
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

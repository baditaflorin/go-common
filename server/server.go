package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
)

type Server struct {
	Config      *config.Config
	Mux         *http.ServeMux
	Middlewares []middleware.Middleware
	Stats       *metrics.Stats
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

// New creates a new Server with optional configuration
func New(cfg *config.Config, opts ...Option) *Server {
	mux := http.NewServeMux()
	stats := metrics.New()

	// Register /health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "healthy",
			"service": cfg.AppName,
			"version": cfg.Version,
		})
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

	srv := &Server{
		Config:      cfg,
		Mux:         mux,
		Middlewares: []middleware.Middleware{},
		Stats:       stats,
	}

	// Apply options
	for _, opt := range opts {
		opt(srv)
	}

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

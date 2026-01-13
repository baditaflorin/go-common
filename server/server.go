package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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

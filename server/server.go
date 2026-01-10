package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/config"
)

type Server struct {
	Config *config.Config
	Mux    *http.ServeMux
}

func New(cfg *config.Config) *Server {
	mux := http.NewServeMux()

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

	// Register /version endpoint (User Request)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(cfg.Version))
	})

	return &Server{Config: cfg, Mux: mux}
}

func (s *Server) Start() {
	addr := ":" + s.Config.Port
	fmt.Printf("Starting %s v%s on %s (DEBUG: /version enabled)\n", s.Config.AppName, s.Config.Version, addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      s.Mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())
}

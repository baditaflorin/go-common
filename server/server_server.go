package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/baditaflorin/go-common/client"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/promx"
	"github.com/baditaflorin/go-common/safehttp"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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

	// promMetricsHandler is the default Prometheus /metrics handler.
	// Start() wraps the final handler so it serves this only when the
	// user has not registered their own /metrics on srv.Mux. Keeps
	// services that already use promhttp.Handler() panic-free.
	promMetricsHandler http.Handler

	// drain holds the graceful-drain configuration when
	// WithGracefulDrain has been applied; nil otherwise. See drain.go
	// for the API + lifecycle. The /readyz endpoint is mounted
	// unconditionally and inspects this field on every request.
	drain *drainConfig

	// maxBodyBytes is the maximum request body size enforced by the
	// body-limit middleware. 0 means no limit (not recommended).
	// Set via WithMaxBodyBytes; default is DefaultMaxBodyBytes.
	maxBodyBytes int64

	// drainTimeout is how long Start() waits for in-flight requests
	// to finish after receiving SIGTERM. Set via WithDrainTimeout.
	drainTimeout time.Duration

	// readTimeout overrides the http.Server ReadTimeout when > 0.
	// Default (0) uses DefaultReadTimeout. Set via WithReadTimeout or
	// WithServerTimeouts. Raise for services that accept slow/large
	// request bodies (streamed uploads).
	readTimeout time.Duration

	// writeTimeout overrides the http.Server WriteTimeout when > 0.
	// Default (0) uses DefaultWriteTimeout. Services that legitimately
	// produce slow responses (e.g. a JS-render escalation through the
	// fleet fetch-cache that can take ~30-60s cold) MUST raise this via
	// WithWriteTimeout — otherwise the Go HTTP server closes the
	// connection mid-response and the gateway surfaces a spurious 502.
	writeTimeout time.Duration

	// idleTimeout overrides the http.Server IdleTimeout when > 0.
	// Default (0) uses DefaultIdleTimeout. Set via WithServerTimeouts.
	idleTimeout time.Duration

	// noRequestStats disables the default reqstats middleware (which emits
	// Server-Timing + X-Request-Stats on every response). Set via
	// WithoutRequestStats; default is enabled.
	noRequestStats bool
}

// Handler returns the fully-wrapped HTTP handler — middleware chain
// applied to s.Mux, plus the fleet-default /metrics and /selftest
// shims. Useful for tests (httptest.NewServer(srv.Handler())) and
// for callers that want to embed the server in a non-stdlib
// listener. Start() uses this internally.
func (s *Server) Handler() http.Handler {
	return s.wrapDefaults(middleware.Chain(s.Mux, s.Middlewares...))
}

// buildHTTPServer constructs the *http.Server with the resolved
// timeouts. Each timeout falls back to its Default* when neither the
// corresponding override field nor the SERVER_*_TIMEOUT_SECONDS env knob
// is set, so existing services are unchanged while WithWriteTimeout /
// WithReadTimeout / WithServerTimeouts (or the env knobs) can raise them.
// Factored out of Start() so the resolution can be unit-tested without
// binding a listener.
func (s *Server) buildHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      h,
		ReadTimeout:  resolveTimeout(s.readTimeout, "SERVER_READ_TIMEOUT_SECONDS", DefaultReadTimeout),
		WriteTimeout: resolveTimeout(s.writeTimeout, "SERVER_WRITE_TIMEOUT_SECONDS", DefaultWriteTimeout),
		IdleTimeout:  resolveTimeout(s.idleTimeout, "SERVER_IDLE_TIMEOUT_SECONDS", DefaultIdleTimeout),
	}
}

// Start listens on PORT, serves requests, and performs a graceful
// shutdown when SIGTERM or SIGINT is received. It blocks until the
// server has drained all in-flight requests or the drain timeout
// elapses, then returns. log.Fatal is no longer used here — callers
// should handle the returned error themselves:
//
//	if err := srv.Start(); err != nil {
//	    log.Fatal(err)
//	}
func (s *Server) Start() error {
	addr := ":" + s.Config.Port
	fmt.Printf("Starting %s v%s on %s\n", s.Config.AppName, s.Config.Version, addr)

	finalHandler := s.wrapDefaults(middleware.Chain(s.Mux, s.Middlewares...))

	httpSrv := s.buildHTTPServer(addr, finalHandler)

	if s.drain != nil {
		// WithGracefulDrain path: wire the shutdown closure so BeginDrain
		// (called by the signal handler, or directly by tests) can drive
		// http.Server.Shutdown on the right *http.Server instance.
		s.drain.shutdownFn = httpSrv.Shutdown
		stopSignals := s.installSignalHandler()
		defer stopSignals()

		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: ListenAndServe error: %v", err)
		}
		// Block until the drain goroutine finishes.
		<-s.drain.shutdownDone
		return nil
	}

	// Default graceful-shutdown path: catch SIGTERM/SIGINT and drain.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: listen error: %w", err)
		}
		return nil
	case sig := <-sigCh:
		log.Printf("server: received %v, draining (timeout %s)…", sig, s.drainTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.drainTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("server: graceful shutdown error: %v", err)
		return err
	}
	log.Printf("server: stopped cleanly")
	return nil
}

// wrapDefaults serves fleet-canonical defaults for /metrics and
// /selftest when the user did not register their own. Detection uses
// http.ServeMux.Handler — if the matched pattern equals the route
// itself, the user registered it and we defer to them through the
// middleware chain. Otherwise we serve the default outside the chain
// (zero contribution to http_requests_total / no auth requirement so
// scrapers and smoke probes Just Work).
//
// /metrics  — promx.Handler() Prometheus text-exposition format.
// /selftest — fleet-contract 200 OK with {service, version, status}.
//
//	Services with real probes should mount selftest.Suite
//	on s.Mux; this default exists so deploy smoke gates
//	never see 400/500 from a catchall handler swallowing
//	the request (the root cause of fleet-runner deploy
//	auto-rollback on services that hadn't opted in).
func (s *Server) wrapDefaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fetch-cache loop guard: a request carrying the one-hop marker is
		// already being served on behalf of the fleet fetch-cache, so disable
		// fetch-cache delegation for any safehttp GET issued while handling it.
		// The fetch then goes direct to origin instead of recursing back into
		// the cache, bounding cache->cache recursion to a single hop.
		if r.Header.Get(fetchCacheHopHeader) != "" {
			r = r.WithContext(safehttp.WithoutFetchCacheContext(r.Context()))
		}
		switch r.URL.Path {
		case "/metrics":
			if s.promMetricsHandler == nil {
				next.ServeHTTP(w, r)
				return
			}
			_, pattern := s.Mux.Handler(r.Clone(r.Context()))
			if pattern == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			s.promMetricsHandler.ServeHTTP(w, r)
			return
		case "/selftest":
			_, pattern := s.Mux.Handler(r.Clone(r.Context()))
			if pattern == "/selftest" {
				next.ServeHTTP(w, r)
				return
			}
			s.defaultSelftest(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// defaultSelftest emits the fleet-contract 200 OK body when no
// selftest.Suite was registered. The note field flags the
// no-implementation case to humans inspecting the response (and to
// go-fleet-selftest-aggregator if it ever wants to surface coverage).
func (s *Server) defaultSelftest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service": s.Config.AppName,
		"version": s.Config.Version,
		"status":  "ok",
		"note":    "no selftest.Suite registered; default 200 handler in go-common/server",
	})
}

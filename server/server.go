package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/client"
	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/metrics"
	"github.com/baditaflorin/go-common/middleware"
	openapipkg "github.com/baditaflorin/go-common/openapi"
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

	// writeTimeout overrides the http.Server WriteTimeout when > 0.
	// Default (0) uses DefaultWriteTimeout. Services that legitimately
	// produce slow responses (e.g. a JS-render escalation through the
	// fleet fetch-cache that can take ~30-60s cold) MUST raise this via
	// WithWriteTimeout — otherwise the Go HTTP server closes the
	// connection mid-response and the gateway surfaces a spurious 502.
	writeTimeout time.Duration
}

// DefaultMaxBodyBytes is the default request body size limit applied
// by server.New unless overridden with WithMaxBodyBytes.
// 4 MiB matches the validate.Bind default and common reverse-proxy limits.
const DefaultMaxBodyBytes = 4 << 20 // 4 MiB

// DefaultDrainTimeout is how long Start waits for in-flight requests
// after receiving SIGTERM before forcing closure.
const DefaultDrainTimeout = 30 * time.Second

// Default http.Server timeouts applied by Start. WriteTimeout is
// overridable per-service via WithWriteTimeout for services that
// legitimately produce slow responses (e.g. a cold JS render).
const (
	DefaultReadTimeout  = 10 * time.Second
	DefaultWriteTimeout = 30 * time.Second
	DefaultIdleTimeout  = 120 * time.Second
)

type Option func(*Server)

// WithMiddleware adds middlewares to the server
func WithMiddleware(mws ...middleware.Middleware) Option {
	return func(s *Server) {
		s.Middlewares = append(s.Middlewares, mws...)
	}
}

// WithMaxBodyBytes sets the maximum request body size. Requests that
// exceed this limit are rejected with HTTP 413 Request Entity Too Large
// before reaching any handler. Default: DefaultMaxBodyBytes (4 MiB).
// Pass 0 to disable the limit (not recommended in production).
func WithMaxBodyBytes(n int64) Option {
	return func(s *Server) { s.maxBodyBytes = n }
}

// WithDrainTimeout sets how long Start() waits for in-flight requests
// to complete after receiving SIGTERM before forcing closure.
// Default: DefaultDrainTimeout (30 s).
func WithDrainTimeout(d time.Duration) Option {
	return func(s *Server) { s.drainTimeout = d }
}

// WithWriteTimeout overrides the http.Server WriteTimeout (the
// per-connection deadline for writing a response, measured from the end
// of the request-header read). Default: DefaultWriteTimeout (30 s).
//
// Raise this for services that legitimately produce slow responses —
// e.g. a JS-render escalation through the fleet fetch-cache, where a
// cold chromedp render of a heavy SPA can take ~30-60 s. Without it the
// Go HTTP server closes the connection at 30 s mid-response and the
// gateway returns a spurious 502 even though the handler completed.
// Pass a value comfortably above the handler's own context budget.
// d <= 0 is ignored (keeps the default).
func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.writeTimeout = d
		}
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

// WithKeystoreAuthMesh is WithKeystoreAuth plus middleware.TrustPrivateMesh:
// a request whose actual TCP peer is a private/loopback/mesh IP and that
// carries no gateway trust header is treated as already-authenticated. Use
// for an internal-only, expensive-to-call service (e.g. the chromedp
// js-proxy) so sibling containers on the docker mesh can reach it no-auth —
// mirroring the fetch cache — while its PUBLIC gateway URL stays fully
// keystore-gated (public clients only ever arrive via nginx, which sets the
// gateway header). localTokens behave exactly as in WithKeystoreAuth.
func WithKeystoreAuthMesh(localTokens ...string) Option {
	return func(s *Server) {
		ks := apikey.NewCache(apikey.New())
		if s.PromAuthCollectors != nil {
			ks.Observer = s.PromAuthCollectors
		}
		s.Middlewares = append(s.Middlewares, middleware.TokenAuthKeystore(middleware.KeystoreOpts{
			Verifier:         ks,
			LocalTokens:      localTokens,
			Observer:         s.PromAuthCollectors,
			TrustPrivateMesh: true,
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
		// Auto-wire the dep observer if AutoWire has already run.
		// Safe to call with nil — depcheck.SetObserver tolerates it.
		if dc := promx.AutoDep(); dc != nil && r != nil {
			r.SetObserver(dc)
		}
	}
}

// WithOpenAPI registers a GET /openapi.json handler that serves spec as JSON.
// The spec is serialised once at startup — mutations to spec after this call
// are not reflected.  Build the spec with openapi.New() and optionally enrich
// it with openapi.ScanDir() before passing it here.
//
//	spec := openapi.New(cfg.AppName, cfg.Version)
//	srv := server.New(cfg, server.WithOpenAPI(spec))
//
// The canonical fleet endpoints (/health, /version, /selftest) are already
// included by openapi.New() — services do not need to add them manually.
func WithOpenAPI(spec *openapipkg.Spec) Option {
	return func(s *Server) {
		data, err := spec.JSON()
		if err != nil {
			// spec is invalid JSON — panic early rather than serve garbage.
			panic("openapi spec serialization failed: " + err.Error())
		}
		s.Mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(data) //nolint:errcheck // client disconnect is not actionable
		})
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
	// 1. Graph observer (outermost: sees final status + latency including
	//    other middleware overhead, records inbound Event)
	// 2. RequestID
	// 3. Logging (injects per-request slog logger into context)
	// 4. BodyLimit (rejects oversized bodies before auth / handler)
	// 5. Metrics (Record Status) — JSON snapshot surface
	// 6. PromHTTP (Record Status) — Prometheus surface
	defaultMWs := []middleware.Middleware{
		graph.Middleware,
		middleware.RequestID,
		middleware.Logging,
	}
	if srv.maxBodyBytes > 0 {
		defaultMWs = append(defaultMWs, bodyLimitMiddleware(srv.maxBodyBytes))
	}
	defaultMWs = append(defaultMWs,
		middleware.Metrics(stats),
		httpColl.Middleware(),
	)
	srv.Middlewares = append(defaultMWs, srv.Middlewares...)

	return srv
}

// Handler returns the fully-wrapped HTTP handler — middleware chain
// applied to s.Mux, plus the fleet-default /metrics and /selftest
// shims. Useful for tests (httptest.NewServer(srv.Handler())) and
// for callers that want to embed the server in a non-stdlib
// listener. Start() uses this internally.
func (s *Server) Handler() http.Handler {
	return s.wrapDefaults(middleware.Chain(s.Mux, s.Middlewares...))
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

	writeTimeout := DefaultWriteTimeout
	if s.writeTimeout > 0 {
		writeTimeout = s.writeTimeout
	}
	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      finalHandler,
		ReadTimeout:  DefaultReadTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  DefaultIdleTimeout,
	}

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

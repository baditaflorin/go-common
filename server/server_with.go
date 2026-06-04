package server

import (
	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/middleware"
	openapipkg "github.com/baditaflorin/go-common/openapi"
	"github.com/baditaflorin/go-common/promx"
	"net/http"
	"time"
)

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

// WithReadTimeout overrides the http.Server ReadTimeout (the deadline
// for reading the entire request, headers + body). Default:
// DefaultReadTimeout (10 s). Raise this for services that accept slow
// or large request bodies (e.g. streamed uploads). d <= 0 is ignored
// (keeps the default).
func WithReadTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.readTimeout = d
		}
	}
}

// WithServerTimeouts sets all three http.Server timeouts in one call:
// ReadTimeout, WriteTimeout, and IdleTimeout. Any argument <= 0 is
// ignored, leaving that timeout at its default
// (DefaultReadTimeout / DefaultWriteTimeout / DefaultIdleTimeout) — so
// callers can raise just one or two by passing 0 for the rest:
//
//	// raise only the write deadline, keep read + idle at defaults
//	server.WithServerTimeouts(0, 70*time.Second, 0)
//
// Equivalent to applying WithReadTimeout, WithWriteTimeout, and a
// matching idle override together.
func WithServerTimeouts(read, write, idle time.Duration) Option {
	return func(s *Server) {
		if read > 0 {
			s.readTimeout = read
		}
		if write > 0 {
			s.writeTimeout = write
		}
		if idle > 0 {
			s.idleTimeout = idle
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

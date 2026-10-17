package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/baditaflorin/go-common/config"
)

// Run is the canonical 0crawl service entrypoint. It bundles the four
// things every fleet service does identically: config load, keystore-
// backed auth, the standard route mounts (/, /<serviceName>, and the
// kebab alias if different), and Start.
//
// A 5-line main.go is the goal:
//
//	package main
//
//	const version = "1.0.0"
//
//	func main() {
//	    server.Run("go_email_extractor", version, Handler)
//	}
//
// What it does, in order:
//
//  1. config.Load(serviceName, version)             — same as today
//  2. WithKeystoreAuth() prepended                 — keystore-only fleet auth
//  3. server.New(cfg, opts...)                      — /health, /version,
//     /metrics + base mw
//  4. srv.Mux.HandleFunc("/", handler)              — catchall (post-gateway
//     strip lands here)
//  5. srv.Mux.HandleFunc("/"+serviceName, handler)  — direct/internal entry
//  6. srv.Mux.HandleFunc("/"+kebab, handler)        — public kebab alias
//     (only if different)
//  7. srv.Start()                                   — listen forever; if it
//     returns an error (the port is taken, a drain timed out) Run logs it and
//     exits with status 1
//
// Callers that need extra routes or custom middleware should use
// server.New(cfg, opts...) + manual srv.Mux.HandleFunc(...) + srv.Start()
// instead of Run. Run is for the 90% of services with no quirks.
func Run(serviceName, version string, handler http.HandlerFunc, opts ...Option) {
	cfg := config.Load(serviceName, version)

	allOpts := append([]Option{WithKeystoreAuth()}, opts...)
	srv := New(cfg, allOpts...)

	srv.Mux.HandleFunc("/", handler)
	srv.Mux.HandleFunc("/"+serviceName, handler)
	if alias := KebabAlias(serviceName); alias != "" && alias != serviceName {
		srv.Mux.HandleFunc("/"+alias, handler)
	}

	serve(srv)
}

// RunTier is Run with the prepended auth swapped from WithKeystoreAuth to
// WithKeystoreAuthTier — for services whose access_tier registry override
// needs enforcing on their own direct entrypoint, not just via the gateway.
// Same 5-line-main.go shape as Run; use this instead of Run + a redundant
// WithKeystoreAuthTier in opts (which would stack two keystore verifies).
//
//	func main() {
//	    server.RunTier("go-pentest-nuclei", version, Handler, "vetted-pentest", true)
//	}
func RunTier(serviceName, version string, handler http.HandlerFunc, requiredTier string, enforce bool, opts ...Option) {
	cfg := config.Load(serviceName, version)

	allOpts := append([]Option{WithKeystoreAuthTier(requiredTier, enforce)}, opts...)
	srv := New(cfg, allOpts...)

	srv.Mux.HandleFunc("/", handler)
	srv.Mux.HandleFunc("/"+serviceName, handler)
	if alias := KebabAlias(serviceName); alias != "" && alias != serviceName {
		srv.Mux.HandleFunc("/"+alias, handler)
	}

	serve(srv)
}

// serve runs srv until it stops. Start returns an error when it cannot listen
// (the port is taken, for example) or when a drain runs out of time. A service
// has nothing left to do after either, so log the error and exit with status 1:
// dropping it let main return, and a supervisor reads exit status 0 as "stopped
// cleanly" (a restart policy of on-failure would not restart the service).
func serve(srv *Server) {
	if err := srv.Start(); err != nil {
		log.Fatalf("%s: %v", srv.Config.AppName, err)
	}
}

// KebabAlias derives the public kebab-case slug from a Go service name.
// "go_email_extractor" → "email-extractor". Returns "" if the input has
// no go_ prefix and no underscores (no transform produced).
//
// Exported so callers that don't use Run can still mount the same
// alias the gateway expects.
func KebabAlias(serviceName string) string {
	trimmed := strings.TrimPrefix(serviceName, "go_")
	kebab := strings.ReplaceAll(trimmed, "_", "-")
	if kebab == serviceName {
		return ""
	}
	return kebab
}

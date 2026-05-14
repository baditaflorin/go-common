package server

import (
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
//   1. config.Load(serviceName, version)             — same as today
//   2. WithKeystoreAuth("default_token") prepended   — fleet auth
//   3. server.New(cfg, opts...)                      — /health, /version,
//                                                      /metrics + base mw
//   4. srv.Mux.HandleFunc("/", handler)              — catchall (post-gateway
//                                                      strip lands here)
//   5. srv.Mux.HandleFunc("/"+serviceName, handler)  — direct/internal entry
//   6. srv.Mux.HandleFunc("/"+kebab, handler)        — public kebab alias
//                                                      (only if different)
//   7. srv.Start()                                   — listen forever
//
// Callers that need extra routes or custom middleware should use
// server.New(cfg, opts...) + manual srv.Mux.HandleFunc(...) + srv.Start()
// instead of Run. Run is for the 90% of services with no quirks.
func Run(serviceName, version string, handler http.HandlerFunc, opts ...Option) {
	cfg := config.Load(serviceName, version)

	allOpts := append([]Option{WithKeystoreAuth("default_token")}, opts...)
	srv := New(cfg, allOpts...)

	srv.Mux.HandleFunc("/", handler)
	srv.Mux.HandleFunc("/"+serviceName, handler)
	if alias := KebabAlias(serviceName); alias != "" && alias != serviceName {
		srv.Mux.HandleFunc("/"+alias, handler)
	}

	srv.Start()
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

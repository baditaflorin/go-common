// resolve.go — env-driven caller-key resolution for fleet consumers.
//
// Why this exists
//
// Every consumer service in the fleet (the ~10 services that *call*
// into mesh-0crawl / mesh-0exec) needs to pick up an API key from
// somewhere at startup. The fleet convention (see private
// fleet-state/OPS.md) is FLEET_API_KEY — populated in every operator
// ~/.zshenv and every container .env. Per-repo overrides are
// expressed as service-specific env vars.
//
// Before this helper, each consumer reimplemented the precedence
// chain. The bug pattern that motivated extracting it: a consumer
// that read only its own SERVICE_*_TOKEN var fell back to literal
// "default_token", which silently 401'd once the universal demo
// token was rotated. Centralising the fallback lets every consumer
// inherit the same resolution without copy-paste.
//
// What it deliberately does NOT do
//
//   - Embed any literal key value. Hardcoding a fallback like
//     "default_token" in a *public* library is a known-weak default:
//     deployments that forget to set FLEET_API_KEY would silently
//     authenticate (until rotation) and the literal value would be
//     auto-discovered by anyone reading the source. Callers that
//     genuinely want a public-demo fallback must opt in *and* log a
//     WARN so the misconfiguration is visible in startup logs.
//
//   - Read or echo the value anywhere except via return. No package
//     state, no log lines containing the key, no debug prints.
//
//   - Validate the key shape. The keystore is the only authority;
//     attempting client-side prefix checks (ak_ vs fb_ vs tok_)
//     would couple consumers to keystore internals.

package apikey

import "os"

// ResolveResult reports the outcome of a Resolve call. Callers
// typically use Source for one INFO log line at startup
// ("authenticating using key from $FLEET_API_KEY") and Found to
// decide whether to fail-fast or fall back to a public-demo token.
type ResolveResult struct {
	// Key is the resolved key value. Empty when Found is false.
	// Callers MUST NOT log this field.
	Key string

	// Source is the name of the env var that supplied Key, or "" if
	// no env var in the chain was set. Safe to log.
	Source string

	// Found is true iff at least one env var in the chain was non-empty.
	Found bool
}

// Resolve walks envVars in order and returns the first non-empty
// value as ResolveResult.Key, with ResolveResult.Source set to the
// env-var name that supplied it. Returns Found=false if every var is
// unset/empty.
//
// Standard fleet ordering for a consumer service:
//
//	r := apikey.Resolve("SERVICE_CATALOG_TOKEN", "FLEET_API_KEY")
//	if !r.Found {
//	    log.Warn("no caller key configured; falling back to public demo token",
//	        "tried", []string{"SERVICE_CATALOG_TOKEN", "FLEET_API_KEY"})
//	    r.Key = "default_token" // explicit opt-in; survives until next rotation
//	}
//	log.Info("apikey resolved", "source", r.Source) // never log r.Key
//
// Resolve performs no I/O beyond os.Getenv and is safe to call from
// any goroutine. Pass the env-var names in priority order; the first
// non-empty wins.
func Resolve(envVars ...string) ResolveResult {
	for _, name := range envVars {
		if v := os.Getenv(name); v != "" {
			return ResolveResult{Key: v, Source: name, Found: true}
		}
	}
	return ResolveResult{}
}

// resolve.go — env-driven caller-key resolution + shape validation
// for fleet consumers calling into mesh-0crawl / mesh-0exec.
//
// Why this exists
//
// Every consumer service in the fleet (the ~10 services that *call*
// into the upstream meshes) needs to pick up an API key from
// somewhere at startup. The fleet convention (see private
// fleet-state/OPS.md) is FLEET_API_KEY — populated in every operator
// ~/.zshenv and every container .env. Per-repo overrides are
// expressed as service-specific env vars.
//
// Before this helper, each consumer reimplemented the precedence
// chain. The bug pattern that motivated extracting it: a consumer
// that read only its own SERVICE_*_TOKEN var fell back to the
// literal "default_token", which silently 401'd once the universal
// demo token was rotated. Centralising the fallback lets every
// consumer inherit the same resolution without copy-paste.
//
// Prefix scheme
//
// Keys minted by go-apikey-service carry KeyPrefixDynamic ("ak_")
// for normal dynamic keys and KeyPrefixFallback ("fb_") for static
// per-service fallbacks. HasFleetPrefix accepts either so consumers
// can detect obvious misconfigurations programmatically — e.g. a
// value of "yes" or "true" or someone's AWS access key pasted into
// FLEET_API_KEY by mistake — at startup, before the first 401 hits
// production traffic.
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
//   - Validate the key against the keystore. HasFleetPrefix is a
//     cheap shape check, not an authentication. The keystore is the
//     only authority on whether a key actually grants access; pair
//     this with apikey.Client.Verify when you need that answer.

package apikey

import (
	"os"
	"strings"
)

// KeyPrefixDynamic is the prefix carried by every key minted via
// /issue (the "normal" dynamic keys: scoped, expiring, revocable).
//
// KeyPrefixFallback is the prefix carried by per-service static
// fallback keys (the break-glass credentials nginx accepts without
// needing the keystore — see fleet-state/OPS.md).
//
// Publishing these constants in a public package is intentional:
// prefixes are configuration contract, not secrets. They tell an
// attacker the *shape* of a fleet key but reveal nothing about any
// specific value (the bytes after the prefix are 64 hex chars of
// cryptographic randomness). The same disclosure already exists in
// the keystore source and any container's `docker inspect`.
const (
	KeyPrefixDynamic  = "ak_"
	KeyPrefixFallback = "fb_"
)

// keyPrefixes is the full set HasFleetPrefix recognises. Adding a
// new prefix here is a contract change for every consumer that
// validates startup config — coordinate with the keystore release
// that mints it.
var keyPrefixes = []string{KeyPrefixDynamic, KeyPrefixFallback}

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
//	} else if !apikey.HasFleetPrefix(r.Key) {
//	    log.Warn("caller key has unrecognised prefix — likely misconfigured",
//	        "source", r.Source,
//	        "expected_prefixes", []string{apikey.KeyPrefixDynamic, apikey.KeyPrefixFallback})
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

// HasFleetPrefix reports whether key carries one of the recognised
// fleet API-key prefixes (KeyPrefixDynamic "ak_" or
// KeyPrefixFallback "fb_"). This is a cheap shape check, not an
// authentication: a key with the right prefix may still be revoked,
// expired, or unknown to the keystore. Use Verify (via Client /
// Cache) for that.
//
// Returns false for the empty string and for the literal
// "default_token" (the public-demo fallback is intentionally
// excluded — operators who fall back to it should see a separate
// WARN, not a silent OK from this validator).
func HasFleetPrefix(key string) bool {
	if key == "" {
		return false
	}
	for _, p := range keyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

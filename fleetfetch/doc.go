// Package fleetfetch is the canonical caller-side client for
// go-infrastructure-fetch-cache.0exec.com — the fleet-wide HTTP fetch
// cache. Use it from any producer that fetches a user-supplied URL
// for analysis (HTML scrapers, structured-data extractors, tech
// fingerprinters, etc.).
//
// Without fleetfetch, every producer in a domainscope-style fanout
// does its own HTTPS fetch of the target URL — 20 producers per
// categorize request = 20× redundant upstream traffic, 20 slightly-
// different snapshots of the page, and 20× chances of being blocked
// or rate-limited by the origin.
//
// With fleetfetch, the first producer to ask for a given URL triggers
// one upstream fetch via the cache; the other 19 receive the same
// cached bytes microseconds later (singleflight collapse on the cache
// side). All 20 producers analyze identical bytes. Origin sees one
// request from the fleet, not twenty.
//
// # Drop-in usage for HTML-fetching producers
//
// Before (each producer does its own fetch):
//
//	client := safehttp.NewClient(
//	    safehttp.WithTimeout(10*time.Second),
//	    safehttp.WithUserAgent(ua.Build(ServiceID, Version)),
//	)
//	resp, err := client.Get(ctx, url)
//
// After (producer routes through the shared cache):
//
//	client := fleetfetch.NewClient(
//	    fleetfetch.WithAPIKey(os.Getenv("FLEET_API_KEY")),
//	)
//	r, err := client.Get(ctx, url)
//	// r.Body is the upstream body; r.Hit / r.AgeSeconds tell you
//	// whether you saved an upstream round-trip.
//
// # When NOT to use fleetfetch
//
//   - Authenticated fetches where the caller supplies session cookies or
//     auth headers (the cache can't safely share those between callers).
//   - Non-idempotent operations (POST, PUT, DELETE).
//   - Crawlers fetching one-shot unique URLs (cache hit rate = 0%).
//   - Live probe semantics where freshness < cache TTL matters
//     (port scans, http3 detection, live RBL lookups).
//
// For those, keep using safehttp directly.
//
// # Resilience
//
// If the cache returns 5xx, rejects the request (4xx with no
// X-FetchCache-* headers), or is unreachable at the transport level
// (connection refused / DNS / no route), fleetfetch transparently
// falls back to a direct safehttp.NewClient fetch (with SSRF guard
// intact). The caller never sees a cache outage as a fetch failure —
// only as a missing hit (Response.ViaFallback, metric result="fallback").
//
// A *slow* cache is treated differently from a *dead* one. When the
// request to the cache exceeds the client timeout — typically a cold
// miss while the cache fetches a slow origin — the default is NOT to
// fall back: direct-fetching the same slow origin would hit the same
// latency, bypass the cache's singleflight de-dup, and register as
// direct egress (proxy bypass). Instead Get returns ErrCacheTimeout
// (metric result="timeout"), which the caller can retry against a
// now-warming cache. Opt into best-effort direct fetch on timeout with
// WithFallbackOnTimeout when a body matters more than avoiding egress.
//
// # Endpoint
//
// Default endpoint is http://go_infrastructure_fetch_cache:18205 —
// the cache container's Docker DNS name, reachable from any other
// container on the same Docker network without auth, TLS, or the
// proxy_egress detour. This is what fleet producers should use.
//
// External callers (outside the fleet network) should override with
// the public gateway URL via env:
//
//	export FLEET_FETCH_CACHE_URL=https://go-infrastructure-fetch-cache.0exec.com
//	export FLEET_FETCH_CACHE_API_KEY=<your-key>
//
// The public path is keystore-gated; the internal path is not (auth
// happens at the gateway, not the upstream container).
package fleetfetch

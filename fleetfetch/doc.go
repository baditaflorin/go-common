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
// If the cache returns 5xx or the request times out, fleetfetch
// transparently falls back to a direct safehttp.NewClient fetch
// (with SSRF guard intact). The caller never sees the cache outage
// as a fetch failure — only as a missing hit.
//
// # Endpoint
//
// Default endpoint is https://go-infrastructure-fetch-cache.0exec.com.
// Override per-process via the FLEET_FETCH_CACHE_URL env var, or per
// client via WithCacheURL.
package fleetfetch

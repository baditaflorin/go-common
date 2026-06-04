package fleetfetch

import (
	"net/http"
	"time"
)

// WithCacheURL overrides the cache endpoint. Default order:
// WithCacheURL → FLEET_FETCH_CACHE_URL env → DefaultURL.
func WithCacheURL(u string) Option {
	return func(c *Client) { c.cacheURL = u }
}

// WithAPIKey sets the X-API-Key header sent to the cache. Default
// order: WithAPIKey → FLEET_FETCH_CACHE_API_KEY env → none. None is
// fine for dockerhost-originated callers (the gateway has an
// internal-allowlist short-circuit).
func WithAPIKey(k string) Option {
	return func(c *Client) { c.apiKey = k }
}

// WithTimeout sets the per-request timeout for both the cache-side
// call and the fallback direct fetch. Default 15s. This bounds how
// long a cold miss (where the cache fetches a slow origin) may take
// before the cache call is considered a timeout — set it generously
// enough that a normal cold-miss upstream fetch completes inside the
// cache call rather than tripping ErrCacheTimeout. See
// WithFallbackOnTimeout for the slow-cache behavior.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithFallbackOnTimeout makes a slow cache (a cache request that
// exceeds the client timeout) fall back to a direct SSRF-safe fetch,
// the same as a dead cache. Off by default: a timeout usually means
// the cache is mid-fetch of a slow origin, so direct-fetching the same
// origin would hit the same latency AND bypass the cache's
// singleflight de-dup. Leaving this off keeps slow-cache events out of
// the "direct egress / proxy bypass" metric (they're reported as
// result="timeout", not result="fallback"). Turn it on only when a
// best-effort body matters more than avoiding redundant egress.
func WithFallbackOnTimeout() Option {
	return func(c *Client) { c.fallbackOnTimeout = true }
}

// WithFallbackClient replaces the default safehttp-based fallback
// client. Provide your own if you need a custom user-agent / proxy /
// observability wrapper on the SSRF-safe path.
func WithFallbackClient(h *http.Client) Option {
	return func(c *Client) { c.fallback = h }
}

// WithRender pins the upstream renderer for every fetch issued by
// this client. Accepts "" (default, direct fetch), "js" (chromedp via
// go-js-proxy), or "html" (Webshare egress via go-html-proxy). When
// the cache's allow/deny policy refuses the requested mode for a given
// host, the cache silently downgrades to direct — the client can
// inspect Response.Header.Get("X-FetchCache-Render") to tell which
// mode actually served.
//
// Default unset → no ?render= query param sent → cache uses its
// pre-v0.2 direct path. Existing producers don't need any code change.
func WithRender(mode string) Option {
	return func(c *Client) { c.render = mode }
}

// WithDefaultHeaders sets headers attached to every fetch issued by
// this client. Per-request headers passed to GetWithHeaders override
// these on a per-name basis. Common use: a fleet-wide User-Agent or
// Accept-Language. Values are forwarded through the cache (and
// therefore included in the cache key) so the cache won't conflate
// requests with different default headers.
func WithDefaultHeaders(h http.Header) Option {
	return func(c *Client) {
		if h == nil {
			c.defaultHeaders = nil
			return
		}
		c.defaultHeaders = h.Clone()
	}
}

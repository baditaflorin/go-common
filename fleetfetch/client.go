package fleetfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/safehttp"
)

// DefaultURL is the canonical fleet fetch-cache endpoint, addressed
// by Docker container DNS so producers in the same Docker network
// reach it without going through the public gateway (no TLS handshake,
// no keystore round-trip, no proxy_egress detour through Webshare).
//
// Override at runtime via the FLEET_FETCH_CACHE_URL env var or per
// client via WithCacheURL. External callers (outside the fleet
// network) should set the env to the public URL:
//
//	FLEET_FETCH_CACHE_URL=https://go-infrastructure-fetch-cache.0exec.com
const DefaultURL = "http://go_infrastructure_fetch_cache:18205"

// PublicURL is the externally-resolvable HTTPS endpoint exposed at
// the gateway. Use it when calling from outside the fleet's Docker
// network. Auth is keystore-gated (X-API-Key or ?api_key=) at this
// path; the internal DefaultURL skips auth because container-to-
// container calls don't traverse the gateway.
const PublicURL = "https://go-infrastructure-fetch-cache.0exec.com"

// EnvCacheURL is the env var name read by NewClient when no
// WithCacheURL is set.
const EnvCacheURL = "FLEET_FETCH_CACHE_URL"

// ForwardHeaderPrefix prefixes any caller-supplied per-request header
// sent to the fleet fetch cache. The cache strips this prefix and
// re-attaches the header to the upstream request, while also folding
// it into the cache key so that different header sets (e.g.
// Accept: application/rdap+json vs. default) don't collide.
const ForwardHeaderPrefix = "X-FF-Forward-"

// EnvAPIKey is the env var name read by NewClient when no WithAPIKey
// is set.
const EnvAPIKey = "FLEET_FETCH_CACHE_API_KEY"

// DefaultAPIKey is the pre-trusted local token (set via
// server.WithKeystoreAuth("default_token") on the cache container).
// NewClient defaults the API key to this when no override is given,
// so internal callers don't need any wiring to satisfy the cache's
// in-process keystore middleware. External callers should override
// via WithAPIKey or FLEET_FETCH_CACHE_API_KEY (the default_token is
// rate-limited at the public gateway).
const DefaultAPIKey = "default_token"

// Response is the result of a fetch. Body is the upstream body
// verbatim; the cache-side metadata is broken out into typed fields
// so callers can log hit rates without parsing headers themselves.
type Response struct {
	Status      int
	Header      http.Header
	Body        []byte
	FinalURL    string
	Hit         bool      // true if served from cache
	AgeSeconds  int       // age of the cached entry; 0 on miss
	UpstreamMS  int64     // time the original upstream fetch took
	FetchedAt   time.Time // when the cached entry was created
	ViaFallback bool      // true if cache was unreachable and we direct-fetched via safehttp

	// Render reports the renderer mode the cache actually honored
	// for this response. May differ from the requested mode if the
	// cache's allow/deny policy downgraded the request (e.g. asked
	// for "js" but the host wasn't on the allow list → cache served
	// "" / default). Empty string for older caches that don't emit
	// X-FetchCache-Render.
	Render string
	// Via reports which renderer produced this response on a cache
	// miss: "direct", "js-proxy", or "html-proxy". Empty on cache
	// hits (the cache doesn't remember which renderer originally
	// served the cached entry) and on older caches.
	Via string
}

// Render mode wire constants. These are the values sent as ?render=<mode>
// to go_infrastructure_fetch_cache v0.2+. Older caches ignore the param
// and serve the default (direct) shape, so clients can opt in without
// coordinating a fleet-wide cache upgrade.
const (
	// RenderDefault sends no render param — the cache uses its
	// SSRF-safe direct fetch (pre-v0.2 behavior). Cheapest; lowest
	// latency; no JS execution.
	RenderDefault = ""
	// RenderJS asks the cache to route through go-js-proxy (chromedp).
	// Returns post-JS DOM. Falls back through go-html-proxy and
	// finally direct if either proxy is unhealthy. Cache key is
	// distinct from default so JS-rendered and curl shapes don't
	// collide.
	RenderJS = "js"
	// RenderHTML asks the cache to route through go-html-proxy
	// (Webshare egress + UA rotation). No JS execution. Cheaper than
	// js but still better than direct for origins that block fleet IPs.
	RenderHTML = "html"
	// RenderJSNetwork asks the cache to route through go-js-proxy-network
	// (chromedp). Returns the post-JS DOM as the body PLUS the full
	// outbound network request log on the X-FetchCache-Network response
	// header. Use FetchNetwork to get the DOM + parsed []NetworkEntry in
	// one call. Cache key is distinct from js/default. Requires
	// go_infrastructure_fetch_cache v0.3+; older caches ignore the param
	// and serve the default shape (no network log).
	RenderJSNetwork = "js-network"
)

// NetworkEntry is one row of the outbound network log captured for a
// JS-rendered page by go-js-proxy-network and surfaced through the fetch
// cache on the X-FetchCache-Network header. The field set + JSON tags match
// go-common/client.NetworkEntry's fingerprint subset: request-fingerprint
// detectors (analytics / martech / error-monitoring) identify a tool by the
// URLs a page calls (GA /g/collect, Segment api.segment.io, Sentry
// *.ingest.sentry.io) even when the loader is bundled/minified — walk these
// entries' URLs to detect them.
type NetworkEntry struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Status       int    `json:"status"`
	ResourceType string `json:"resource_type"`
	Initiator    string `json:"initiator"`
}

// NetworkHeader is the response header the fetch cache emits (js-network
// mode) carrying the JSON-encoded network log. Exported so consumers that
// hold a raw *Response can re-parse it without re-deriving the name.
const NetworkHeader = "X-FetchCache-Network"

// Client wraps the HTTP plumbing for talking to the fleet fetch cache,
// with transparent fallback to a SSRF-safe direct fetch when the cache
// is unreachable. Safe for concurrent use.
type Client struct {
	cacheURL    string
	apiKey      string
	cacheClient *http.Client // HTTP client used to talk to the cache itself
	fallback    *http.Client // SSRF-safe client used when cache is down
	timeout     time.Duration
	render      string // "" | "js" | "html"; forwarded as ?render=<mode>

	// defaultHeaders are sent on every Get; per-request headers passed
	// to GetWithHeaders are merged on top (per-request wins).
	defaultHeaders http.Header

	// fallbackOnTimeout controls what happens when the cache is
	// reachable but too slow (the request to the cache exceeds
	// `timeout`). Default false: a slow cache is NOT treated like a
	// dead one — we surface ErrCacheTimeout instead of direct-fetching
	// the same (likely-slow) origin and bypassing the cache's
	// singleflight. Set true via WithFallbackOnTimeout to opt into
	// best-effort direct fetch on timeout.
	fallbackOnTimeout bool

	// noCache, when set via WithoutCache, makes every Get bypass the
	// fetch cache entirely and go straight to the SSRF-safe, proxy-aware
	// direct fetch (the same path directFetch uses). For callers that
	// probe speculative / one-shot URLs not worth caching — they
	// shouldn't pay the cache round-trip OR pollute the shared cache
	// (and its singleflight) with throwaway lookups.
	noCache bool

	// Counters for observability. Exposed via Stats().
	hits      atomic.Int64
	misses    atomic.Int64
	fallbacks atomic.Int64
	timeouts  atomic.Int64
	errs      atomic.Int64
}

// ErrCacheTimeout is returned by Get when the call to the fetch cache
// exceeds the client timeout and WithFallbackOnTimeout was not set. It
// signals "the cache is reachable but slow" (typically a cold miss on
// a slow origin), as distinct from "the cache is down" (which still
// transparently falls back to a direct fetch). Callers can retry — a
// second attempt usually lands on a now-warm cache entry.
var ErrCacheTimeout = errors.New("fleetfetch: cache request timed out")

// Option configures a Client.
type Option func(*Client)

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

// WithoutCache makes the client skip the fetch cache entirely: every Get
// goes straight to the SSRF-safe, proxy-aware direct fetch (via the
// fallback client, which honors HTTP(S)_PROXY for proxy_egress services).
//
// Use for services that probe speculative, mostly-nonexistent, or
// one-shot URLs — e.g. a docs-platform detector guessing developer.<dom>.com
// for every input. Routing those through the cache pays a Docker-internal
// round-trip AND pollutes the shared cache + its singleflight with
// throwaway lookups that no other service will ever reuse. WithoutCache
// turns the call into a single direct fetch instead.
//
// The Response shape is unchanged (Body/Status/FinalURL/Header), with
// ViaFallback=true to mark that it took the direct path. WithRender has
// no effect under WithoutCache — rendering requires the cache's headless
// renderer; a direct fetch returns raw origin bytes. Cacheable, shareable
// work (e.g. a JS render worth warming once per domain) should keep using
// a normal cache client.
func WithoutCache() Option {
	return func(c *Client) { c.noCache = true }
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

// NewClient returns a Client wired with sensible defaults. Reads
// FLEET_FETCH_CACHE_URL and FLEET_FETCH_CACHE_API_KEY from the env
// when corresponding options aren't given.
func NewClient(opts ...Option) *Client {
	c := &Client{
		timeout: 15 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	if c.cacheURL == "" {
		if env := os.Getenv(EnvCacheURL); env != "" {
			c.cacheURL = env
		} else {
			c.cacheURL = DefaultURL
		}
	}
	if c.apiKey == "" {
		c.apiKey = os.Getenv(EnvAPIKey)
	}
	if c.apiKey == "" {
		c.apiKey = DefaultAPIKey
	}
	if c.cacheClient == nil {
		// Proxy: nil is load-bearing here.
		//
		// Services with proxy_egress: true in service.yaml have HTTPS_PROXY
		// (and sometimes HTTP_PROXY) injected into their container environment
		// from /opt/_shared/proxy.env at deploy time. Go's default transport
		// picks those vars up via http.ProxyFromEnvironment, so a bare
		// &http.Client{} would route the cache call through Webshare.
		//
		// The cache URL is a Docker-internal hostname (go_infrastructure_fetch_cache)
		// that is invisible to any external proxy — Webshare cannot resolve it and
		// returns target_connect_resolve_failed after a 21-second timeout.
		//
		// Proxy: nil disables env-based proxy lookup for this transport only.
		// Public-internet enrichment calls use a separate client (the fallback
		// field) which correctly inherits the proxy settings.
		c.cacheClient = &http.Client{
			Transport: &http.Transport{Proxy: nil},
			Timeout:   c.timeout,
		}
	}
	if c.fallback == nil {
		c.fallback = safehttp.NewClient(safehttp.WithTimeout(c.timeout))
	}
	return c
}

// Get fetches targetURL through the cache. On a cache 5xx/network
// failure, falls back to a direct SSRF-safe fetch.
func (c *Client) Get(ctx context.Context, targetURL string) (*Response, error) {
	return c.fetch(ctx, targetURL, 0, nil, c.render)
}

// GetWithMaxAge is Get with an explicit max-age override (in seconds
// on the wire). maxAge=0 = use cache default (60s).
func (c *Client) GetWithMaxAge(ctx context.Context, targetURL string, maxAge time.Duration) (*Response, error) {
	return c.fetch(ctx, targetURL, maxAge, nil, c.render)
}

// GetWithHeaders is Get with per-request headers forwarded to the
// upstream fetch. Headers are sent to the cache prefixed with
// ForwardHeaderPrefix; the cache strips the prefix, attaches them to
// the upstream request, and includes them in the cache key so
// requests with different header sets don't share an entry.
//
// Default headers configured via WithDefaultHeaders are merged in
// first; per-request headers win on collisions. Pass nil to get the
// same behavior as Get.
func (c *Client) GetWithHeaders(ctx context.Context, targetURL string, headers http.Header) (*Response, error) {
	return c.fetch(ctx, targetURL, 0, headers, c.render)
}

// GetRendered fetches targetURL with a per-call render override. Pass
// RenderJS / RenderHTML / RenderDefault. Useful when the client-wide
// default doesn't fit a particular call (e.g. a JS-rendering client
// that needs a single cheap default-mode lookup).
func (c *Client) GetRendered(ctx context.Context, targetURL string, mode string) (*Response, error) {
	return c.fetch(ctx, targetURL, 0, nil, mode)
}

// FetchNetwork fetches targetURL with render=js-network and returns the
// rendered DOM (Response.Body) together with the page's outbound network
// request log parsed from the X-FetchCache-Network header. One render per
// (url, js-network) is shared fleet-wide via the cache, so the FIRST caller
// of a given domain pays the cold-render cost (up to ~60s) and every
// subsequent caller gets a cache hit (~ms) with the same log replayed.
//
// This is the consumer-facing API for request-fingerprint detection: a
// detector calls FetchNetwork(domain) and walks the returned []NetworkEntry
// URLs for tool signatures (GA /g/collect, api.segment.io, *.ingest.sentry.io).
//
// The returned slice is nil (not an error) when the cache served a
// non-network shape — e.g. an older cache that doesn't honor js-network, or
// a fallback to a DOM-only renderer when go-js-proxy-network was unhealthy.
// Callers should treat a nil/empty log as "no network signal available",
// not as a hard failure (the DOM in Response.Body is still usable). Inspect
// Response.Render / Response.Via to tell whether the network renderer
// actually served the request.
//
// On cache-unreachable fallback (Response.ViaFallback == true) there is no
// network log — the direct SSRF fetch can't capture one — so the slice is
// nil. The error is non-nil only on a genuine fetch failure.
func (c *Client) FetchNetwork(ctx context.Context, targetURL string) (*Response, []NetworkEntry, error) {
	resp, err := c.fetch(ctx, targetURL, 0, nil, RenderJSNetwork)
	if err != nil {
		return resp, nil, err
	}
	return resp, parseNetworkLog(resp.Header.Get(NetworkHeader)), nil
}

// parseNetworkLog decodes the X-FetchCache-Network header value (a JSON
// array) into []NetworkEntry. Returns nil for an empty/absent header or
// invalid JSON — the network log is advisory telemetry, so a malformed
// header must never fail the caller's fetch (they still have the DOM).
func parseNetworkLog(raw string) []NetworkEntry {
	if raw == "" || raw == "[]" {
		return nil
	}
	var entries []NetworkEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

// fetch is the shared implementation behind Get/GetWithMaxAge/GetWithHeaders.
func (c *Client) fetch(ctx context.Context, targetURL string, maxAge time.Duration, perReqHeaders http.Header, render string) (fetchRes *Response, retErr error) {
	if targetURL == "" {
		return nil, errors.New("fleetfetch: empty target url")
	}
	start := time.Now()
	defer func() {
		ev := Event{Host: hostOf(targetURL), Duration: time.Since(start)}
		switch {
		case errors.Is(retErr, ErrCacheTimeout):
			ev.Result = "timeout"
		case retErr != nil:
			ev.Result = "error"
		case fetchRes == nil:
			ev.Result = "error"
		case fetchRes.ViaFallback:
			ev.Result = "fallback"
			ev.Status = fetchRes.Status
		case fetchRes.Hit:
			ev.Result = "hit"
			ev.Status = fetchRes.Status
			ev.AgeSeconds = fetchRes.AgeSeconds
		default:
			ev.Result = "miss"
			ev.Status = fetchRes.Status
		}
		emit(ev)
	}()
	merged := mergeHeaders(c.defaultHeaders, perReqHeaders)

	// WithoutCache: never touch the cache — fetch direct via the
	// proxy-aware fallback client. Emits result="fallback" with
	// ViaFallback=true (it IS a direct fetch), keeping these throwaway
	// probes out of the cache and its singleflight entirely.
	if c.noCache {
		return c.directFetch(ctx, targetURL, merged, nil)
	}

	q := url.Values{}
	q.Set("url", targetURL)
	if maxAge > 0 {
		q.Set("max_age", strconv.FormatInt(int64(maxAge.Seconds()), 10))
	}
	if render != "" {
		// Older caches (<v0.2) ignore unknown query params, so this is
		// always safe to send — they'll just serve the default shape.
		q.Set("render", render)
	}
	reqURL := c.cacheURL + "/fetch?" + q.Encode()

	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, reqURL, nil)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("fleetfetch: build cache request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set(header.APIKey, c.apiKey)
	}
	for name, vals := range merged {
		for _, v := range vals {
			req.Header.Add(ForwardHeaderPrefix+name, v)
		}
	}

	resp, err := c.cacheClient.Do(req)
	if err != nil {
		// Caller's own context expired/cancelled — they gave up.
		// Don't fall back or direct-fetch; just propagate.
		if ctx.Err() != nil {
			c.errs.Add(1)
			return nil, fmt.Errorf("fleetfetch: caller context done: %w", ctx.Err())
		}
		// Cache reachable but slow (our per-request deadline fired, not
		// a transport failure). A direct fetch of the same origin would
		// hit the same latency and bypass singleflight, so by default
		// we surface a timeout instead of bypassing the cache.
		if isTimeout(err) {
			c.timeouts.Add(1)
			if !c.fallbackOnTimeout {
				c.errs.Add(1)
				return nil, fmt.Errorf("%w: %v", ErrCacheTimeout, err)
			}
			return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache timeout: %w", err))
		}
		// Genuine transport failure (refused / DNS / no route): the
		// cache is unreachable, so a direct fetch is the right
		// degradation.
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache transport: %w", err))
	}
	defer resp.Body.Close()

	// Cache returned a 5xx → fallback.
	if resp.StatusCode >= 500 {
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache status %d", resp.StatusCode))
	}
	// Cache returned a 4xx but the response has no X-FetchCache-*
	// headers → the cache itself rejected us (auth, malformed input,
	// rate limit), not an upstream-passed 4xx. Fall back so the
	// producer still gets a real response.
	if resp.StatusCode >= 400 && resp.Header.Get("X-FetchCache-Fetched-At") == "" {
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache rejected with status %d (no X-FetchCache headers)", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache body: %w", err))
	}

	out := &Response{
		Status:   resp.StatusCode,
		Header:   resp.Header.Clone(),
		Body:     body,
		FinalURL: resp.Header.Get("X-FetchCache-Final-Url"),
	}
	out.Hit, _ = strconv.ParseBool(resp.Header.Get("X-FetchCache-Hit"))
	if v := resp.Header.Get("X-FetchCache-Age-Seconds"); v != "" {
		out.AgeSeconds, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-FetchCache-Upstream-Ms"); v != "" {
		out.UpstreamMS, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("X-FetchCache-Fetched-At"); v != "" {
		out.FetchedAt, _ = time.Parse(time.RFC3339, v)
	}
	out.Render = resp.Header.Get("X-FetchCache-Render")
	out.Via = resp.Header.Get("X-FetchCache-Via")
	if out.FinalURL == "" {
		out.FinalURL = targetURL
	}

	if out.Hit {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return out, nil
}

// directFetch is the fallback path when the cache is unreachable.
// Uses the SSRF-safe fallback client to fetch targetURL directly
// from origin. Returns a Response with ViaFallback=true.
func (c *Client) directFetch(ctx context.Context, targetURL string, headers http.Header, cacheErr error) (*Response, error) {
	c.fallbacks.Add(1)

	fctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, targetURL, nil)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("fleetfetch: fallback build (cache=%v): %w", cacheErr, err)
	}
	for name, vals := range headers {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	resp, err := c.fallback.Do(req)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("fleetfetch: fallback transport (cache=%v): %w", cacheErr, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("fleetfetch: fallback body (cache=%v): %w", cacheErr, err)
	}

	final := targetURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &Response{
		Status:      resp.StatusCode,
		Header:      resp.Header.Clone(),
		Body:        body,
		FinalURL:    final,
		Hit:         false,
		ViaFallback: true,
		FetchedAt:   time.Now().UTC(),
	}, nil
}

// Stats returns a snapshot of the client's lifetime counters. Useful
// for /metrics exposition or debug endpoints.
type Stats struct {
	Hits      int64
	Misses    int64
	Fallbacks int64
	Timeouts  int64
	Errors    int64
}

func (c *Client) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Fallbacks: c.fallbacks.Load(),
		Timeouts:  c.timeouts.Load(),
		Errors:    c.errs.Load(),
	}
}

// isTimeout reports whether err represents a deadline/timeout (as
// opposed to a connection-level transport failure). Used to tell a
// slow-but-reachable cache apart from a dead one.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// mergeHeaders returns base ∪ overlay with overlay overriding base on
// per-name collisions. Either side may be nil. Returned map is safe
// for the caller to mutate.
func mergeHeaders(base, overlay http.Header) http.Header {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := http.Header{}
	for k, vs := range base {
		out[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	for k, vs := range overlay {
		out[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	return out
}

package fleetfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
)

// PublicURL is the externally-resolvable HTTPS endpoint exposed at
// the gateway. Use it when calling from outside the fleet's Docker
// network. Auth is keystore-gated (X-API-Key or ?api_key=) at this
// path; the internal DefaultURL skips auth because container-to-
// container calls don't traverse the gateway.
const PublicURL = "https://go-infrastructure-fetch-cache.0exec.com"

// ForwardHeaderPrefix prefixes any caller-supplied per-request header
// sent to the fleet fetch cache. The cache strips this prefix and
// re-attaches the header to the upstream request, while also folding
// it into the cache key so that different header sets (e.g.
// Accept: application/rdap+json vs. default) don't collide.
const ForwardHeaderPrefix = "X-FF-Forward-"

// fetchCacheHopHeader is internal cache-control metadata used by server's
// loop guard. It must travel to the cache itself, not to the public origin
// and not through X-FF-Forward-* (where it would fragment cache identity).
const fetchCacheHopHeader = "X-Fetch-Cache-Hop"

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

// ErrCacheTimeout is returned by Get when the call to the fetch cache
// exceeds the client timeout and WithFallbackOnTimeout was not set. It
// signals "the cache is reachable but slow" (typically a cold miss on
// a slow origin), as distinct from "the cache is down" (which still
// transparently falls back to a direct fetch). Callers can retry — a
// second attempt usually lands on a now-warm cache entry.
var (
	ErrCacheTimeout = errors.New("fleetfetch: cache request timed out")

	// ErrCacheAuth is returned when the cache itself rejects this client's
	// credential with 401 or 403. It is terminal for the Client: because the
	// API key cannot change after construction, the client opens an auth
	// circuit and subsequent calls fail fast until a new Client is created.
	ErrCacheAuth = errors.New("fleetfetch: cache authentication rejected")

	// ErrRenderBusy is the sentinel wrapped by *RenderBusyError when the
	// fetch cache explicitly sheds a rendered request. It is distinct from
	// a cache transport outage and from an upstream 503 replayed by the
	// cache. Callers may use errors.Is(err, ErrRenderBusy), then errors.As
	// to inspect Retry-After without parsing an error string.
	ErrRenderBusy = errors.New("fleetfetch: renderer at capacity")
)

// CacheAuthError describes a cache-side 401/403. CircuitOpen is true on
// calls rejected locally after an earlier authentication failure; false on
// the first response received from the cache.
type CacheAuthError struct {
	StatusCode  int
	CircuitOpen bool
}

func (e *CacheAuthError) Error() string {
	if e == nil {
		return ErrCacheAuth.Error()
	}
	if e.CircuitOpen {
		return fmt.Sprintf("%s (http %d; circuit open)", ErrCacheAuth, e.StatusCode)
	}
	return fmt.Sprintf("%s (http %d)", ErrCacheAuth, e.StatusCode)
}

// Unwrap makes errors.Is(err, ErrCacheAuth) work while errors.As retains
// the status and whether the error came from the open circuit.
func (e *CacheAuthError) Unwrap() error { return ErrCacheAuth }

// RenderBusyError preserves the structured load-shed response returned by
// the fetch cache. RetryAfter is the exact wire value (seconds or HTTP-date)
// so a coordinator can propagate it without losing information.
type RenderBusyError struct {
	StatusCode int
	RetryAfter string
	Message    string
}

func (e *RenderBusyError) Error() string {
	if e == nil {
		return ErrRenderBusy.Error()
	}
	detail := e.Message
	if detail == "" {
		detail = ErrRenderBusy.Error()
	}
	if e.RetryAfter != "" {
		return fmt.Sprintf("%s (http %d, retry-after %s)", detail, e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("%s (http %d)", detail, e.StatusCode)
}

// Unwrap makes errors.Is(err, ErrRenderBusy) work while retaining typed
// Retry-After details through errors.As.
func (e *RenderBusyError) Unwrap() error { return ErrRenderBusy }

// RetryDelay converts Retry-After into a non-negative delay. Both forms
// allowed by RFC 9110 are accepted: integer seconds and an HTTP date.
func (e *RenderBusyError) RetryDelay(now time.Time) (time.Duration, bool) {
	if e == nil || strings.TrimSpace(e.RetryAfter) == "" {
		return 0, false
	}
	value := strings.TrimSpace(e.RetryAfter)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

// Option configures a Client.
type Option func(*Client)

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
//
// Observability: these fetches emit fleet_fetch_total with result="direct"
// (returned a response) or "direct_error" (the direct fetch failed) — never
// "hit"/"miss"/"fallback"/"error", so by-design cache-bypass traffic is
// always distinguishable from real cache activity on a dashboard.
func WithoutCache() Option {
	return func(c *Client) { c.noCache = true }
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
		// A fleetfetch fallback must never resolve safehttp's process-wide
		// fetch-cache delegate: doing so re-enters this Client when the cache
		// is unavailable or rejects a request. directFetch also stamps the
		// per-request context opt-out so caller-provided safehttp fallbacks get
		// the same protection.
		c.fallback = safehttp.NewClient(
			safehttp.WithTimeout(c.timeout),
			safehttp.WithoutFetchCache(),
		)
	}
	return c
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

// Stats returns a snapshot of the client's lifetime counters. Useful
// for /metrics exposition or debug endpoints.
type Stats struct {
	Hits      int64
	Misses    int64
	Fallbacks int64
	Timeouts  int64
	Errors    int64
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

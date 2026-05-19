package fleetfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

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
	Status     int
	Header     http.Header
	Body       []byte
	FinalURL   string
	Hit        bool          // true if served from cache
	AgeSeconds int           // age of the cached entry; 0 on miss
	UpstreamMS int64         // time the original upstream fetch took
	FetchedAt  time.Time     // when the cached entry was created
	ViaFallback bool         // true if cache was unreachable and we direct-fetched via safehttp
}

// Client wraps the HTTP plumbing for talking to the fleet fetch cache,
// with transparent fallback to a SSRF-safe direct fetch when the cache
// is unreachable. Safe for concurrent use.
type Client struct {
	cacheURL    string
	apiKey      string
	cacheClient *http.Client     // HTTP client used to talk to the cache itself
	fallback    *http.Client     // SSRF-safe client used when cache is down
	timeout     time.Duration

	// defaultHeaders are sent on every Get; per-request headers passed
	// to GetWithHeaders are merged on top (per-request wins).
	defaultHeaders http.Header

	// Counters for observability. Exposed via Stats().
	hits       atomic.Int64
	misses     atomic.Int64
	fallbacks  atomic.Int64
	errs       atomic.Int64
}

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
// call and the fallback direct fetch. Default 15s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithFallbackClient replaces the default safehttp-based fallback
// client. Provide your own if you need a custom user-agent / proxy /
// observability wrapper on the SSRF-safe path.
func WithFallbackClient(h *http.Client) Option {
	return func(c *Client) { c.fallback = h }
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
		c.cacheClient = &http.Client{Timeout: c.timeout}
	}
	if c.fallback == nil {
		c.fallback = safehttp.NewClient(safehttp.WithTimeout(c.timeout))
	}
	return c
}

// Get fetches targetURL through the cache. On a cache 5xx/network
// failure, falls back to a direct SSRF-safe fetch.
func (c *Client) Get(ctx context.Context, targetURL string) (*Response, error) {
	return c.fetch(ctx, targetURL, 0, nil)
}

// GetWithMaxAge is Get with an explicit max-age override (in seconds
// on the wire). maxAge=0 = use cache default (60s).
func (c *Client) GetWithMaxAge(ctx context.Context, targetURL string, maxAge time.Duration) (*Response, error) {
	return c.fetch(ctx, targetURL, maxAge, nil)
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
	return c.fetch(ctx, targetURL, 0, headers)
}

// fetch is the shared implementation behind Get/GetWithMaxAge/GetWithHeaders.
func (c *Client) fetch(ctx context.Context, targetURL string, maxAge time.Duration, perReqHeaders http.Header) (*Response, error) {
	if targetURL == "" {
		return nil, errors.New("fleetfetch: empty target url")
	}
	merged := mergeHeaders(c.defaultHeaders, perReqHeaders)

	q := url.Values{}
	q.Set("url", targetURL)
	if maxAge > 0 {
		q.Set("max_age", strconv.FormatInt(int64(maxAge.Seconds()), 10))
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
		req.Header.Set("X-API-Key", c.apiKey)
	}
	for name, vals := range merged {
		for _, v := range vals {
			req.Header.Add(ForwardHeaderPrefix+name, v)
		}
	}

	resp, err := c.cacheClient.Do(req)
	if err != nil {
		// Network failure talking to the cache → fallback.
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
	Errors    int64
}

func (c *Client) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Fallbacks: c.fallbacks.Load(),
		Errors:    c.errs.Load(),
	}
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

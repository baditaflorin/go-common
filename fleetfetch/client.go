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

// DefaultURL is the canonical fleet fetch-cache endpoint. Overridable
// at runtime via the FLEET_FETCH_CACHE_URL env var or per-client via
// WithCacheURL.
const DefaultURL = "https://go-infrastructure-fetch-cache.0exec.com"

// EnvCacheURL is the env var name read by NewClient when no
// WithCacheURL is set.
const EnvCacheURL = "FLEET_FETCH_CACHE_URL"

// EnvAPIKey is the env var name read by NewClient when no WithAPIKey
// is set. Optional: dockerhost-originated calls hit the gateway's
// internal-allowlist short-circuit and don't need a key.
const EnvAPIKey = "FLEET_FETCH_CACHE_API_KEY"

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
	return c.GetWithMaxAge(ctx, targetURL, 0)
}

// GetWithMaxAge is Get with an explicit max-age override (in seconds
// on the wire). maxAge=0 = use cache default (60s).
func (c *Client) GetWithMaxAge(ctx context.Context, targetURL string, maxAge time.Duration) (*Response, error) {
	if targetURL == "" {
		return nil, errors.New("fleetfetch: empty target url")
	}
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

	resp, err := c.cacheClient.Do(req)
	if err != nil {
		// Network failure talking to the cache → fallback.
		return c.directFetch(ctx, targetURL, fmt.Errorf("cache transport: %w", err))
	}
	defer resp.Body.Close()

	// Cache returned a 5xx → fallback.
	if resp.StatusCode >= 500 {
		return c.directFetch(ctx, targetURL, fmt.Errorf("cache status %d", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.directFetch(ctx, targetURL, fmt.Errorf("cache body: %w", err))
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
func (c *Client) directFetch(ctx context.Context, targetURL string, cacheErr error) (*Response, error) {
	c.fallbacks.Add(1)

	fctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, targetURL, nil)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("fleetfetch: fallback build (cache=%v): %w", cacheErr, err)
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

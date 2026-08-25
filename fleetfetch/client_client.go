package fleetfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/peektrace"
	"github.com/baditaflorin/go-common/safehttp"
)

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

	// caller is the X-Fleet-Caller value this client sends to the cache,
	// identifying the calling service so downstream renderers (go-js-proxy)
	// can attribute render load per-enricher. Empty = fall back to the
	// process default (SetDefaultCaller) then env; see resolveCaller.
	caller string

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

	// authRejected is a permanent, per-client circuit opened by a cache-side
	// 401/403. The API key is immutable after NewClient, so retrying the same
	// credentials on every origin request can only hammer the cache. A new
	// client (normally a process restart after credential rotation) starts
	// with a closed circuit.
	authRejected atomic.Int32
}

// Get fetches targetURL through the cache. Genuine cache-service/network
// failures fall back to a direct SSRF-safe fetch. Cached upstream failures
// are returned as-is, and typed load shedding for rendered modes returns
// ErrRenderBusy rather than silently substituting unrendered bytes.
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

// fetch is the shared implementation behind Get/GetWithMaxAge/GetWithHeaders.
func (c *Client) fetch(ctx context.Context, targetURL string, maxAge time.Duration, perReqHeaders http.Header, render string) (fetchRes *Response, retErr error) {
	if targetURL == "" {
		return nil, errors.New("fleetfetch: empty target url")
	}
	start := time.Now()
	defer func() {
		ev := Event{Host: hostOf(targetURL), Duration: time.Since(start)}
		switch {
		// WithoutCache clients never touch the cache — tag their direct
		// fetches distinctly so they don't read as cache hits/errors on a
		// cache dashboard. "direct" = the direct fetch returned a response;
		// "direct_error" = the direct fetch itself failed (DNS / transport /
		// timeout to origin, common when probing speculative URLs).
		case c.noCache && retErr != nil:
			ev.Result = "direct_error"
		case c.noCache:
			ev.Result = "direct"
			if fetchRes != nil {
				ev.Status = fetchRes.Status
			}
		case errors.Is(retErr, ErrCacheTimeout):
			ev.Result = "timeout"
		case errors.Is(retErr, ErrRenderBusy):
			ev.Result = "busy"
			ev.Status = http.StatusServiceUnavailable
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
	// The server loop marker controls the cache request itself. Keeping it
	// out of merged prevents both public-header leakage and a unique
	// X-FF-Forward-* cache-key cohort for delegated requests.
	cacheHop := merged.Get(fetchCacheHopHeader)
	merged.Del(fetchCacheHopHeader)

	// WithoutCache: never touch the cache — fetch direct via the
	// proxy-aware fallback client. Emits result="direct" / "direct_error"
	// (NOT "fallback"/"error") so a cache dashboard never misreads these
	// by-design direct probes as cache traffic. ViaFallback stays true on
	// the Response (the body came via the direct path). Keeps these
	// throwaway probes out of the cache and its singleflight entirely.
	if c.noCache {
		return c.directFetch(ctx, targetURL, merged, nil)
	}

	// Authentication failures are configuration failures, not cache
	// availability failures. Once this client has seen a 401/403, fail fast
	// without repeatedly presenting the same immutable credential. The
	// safehttp process-wide delegate treats this error as a signal to use its
	// own direct transport, while direct fleetfetch callers get a typed error
	// that makes the bad configuration visible.
	if status := int(c.authRejected.Load()); status != 0 {
		c.errs.Add(1)
		return nil, &CacheAuthError{StatusCode: status, CircuitOpen: true}
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
	// Identify the calling service to the cache so it can forward
	// X-Fleet-Caller to go-js-proxy / go-html-proxy for per-enricher load
	// attribution. Set ONLY on the internal cache request — never on the
	// direct-to-origin fallback (directFetch), which would leak an internal
	// header to a public site. Empty caller → header omitted (no regression).
	if caller := c.resolveCaller(); caller != "" {
		req.Header.Set(header.FleetCaller, caller)
	}
	// Trace headers are attached only to the internal cache request. The
	// cache consumes them for its temporary report and never forwards them to
	// the public origin. They are intentionally not X-FF-Forward-* headers,
	// so they cannot affect cache identity.
	peektrace.ApplyHeaders(ctx, req.Header)
	if cacheHop != "" {
		req.Header.Set(fetchCacheHopHeader, cacheHop)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache body: %w", err))
	}

	// A fetched-at marker means the cache is replaying an origin result,
	// including a negative-cached 5xx. Preserve it exactly; direct-fetching
	// here defeats negative caching and multiplies origin load per caller.
	fetchedAt := resp.Header.Get("X-FetchCache-Fetched-At")
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && fetchedAt == "" {
		c.authRejected.CompareAndSwap(0, int32(resp.StatusCode))
		c.errs.Add(1)
		return nil, &CacheAuthError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode >= 500 && fetchedAt == "" {
		if message, shed := loadshed.DecodeShed(resp.StatusCode, body); shed {
			busyErr := &RenderBusyError{
				StatusCode: resp.StatusCode,
				RetryAfter: resp.Header.Get("Retry-After"),
				Message:    message,
			}
			// A static/default fetch can degrade to a semantically equivalent
			// direct request. Rendered modes cannot: doing so silently replaces
			// post-JS evidence with raw HTML and creates unshared origin work.
			if render != RenderDefault {
				c.errs.Add(1)
				return nil, busyErr
			}
			return c.directFetch(ctx, targetURL, merged, busyErr)
		}
		// Preserve the historical fallback for genuine cache-service 5xx
		// responses that are neither origin replay nor typed load shedding.
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache status %d", resp.StatusCode))
	}
	// Cache returned a 4xx but the response has no X-FetchCache-*
	// headers → the cache itself rejected us (auth, malformed input,
	// rate limit), not an upstream-passed 4xx. Fall back so the
	// producer still gets a real response.
	if resp.StatusCode >= 400 && fetchedAt == "" {
		return c.directFetch(ctx, targetURL, merged, fmt.Errorf("cache rejected with status %d (no X-FetchCache headers)", resp.StatusCode))
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
	if fetchedAt != "" {
		out.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt)
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
//
// Deliberately opens its OWN fresh c.timeout window rather than
// sharing whatever remains of the ctx passed into fetch — a call that
// times out against the cache and then falls back here can cost up to
// 2×c.timeout in total. See the WithTimeout doc comment in
// client_with.go for the full rationale and the two existing tests
// that pin this as intended, not a bug.
func (c *Client) directFetch(ctx context.Context, targetURL string, headers http.Header, cacheErr error) (*Response, error) {
	c.fallbacks.Add(1)

	fctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// This is already the cache's degradation path. A normal safehttp client
	// resolves the process-wide fetch delegate at RoundTrip time, so without
	// this request-level opt-out it can route straight back into this same
	// fleetfetch client (cache rejects -> fallback -> delegate -> cache ...).
	// Keep the guard even though NewClient's built-in fallback also opts out:
	// callers may supply an ordinary safehttp client via WithFallbackClient.
	fctx = safehttp.WithoutFetchCacheContext(fctx)

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

func (c *Client) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Fallbacks: c.fallbacks.Load(),
		Timeouts:  c.timeouts.Load(),
		Errors:    c.errs.Load(),
	}
}

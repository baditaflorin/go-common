// fetchproxy.go is the unified fleet-internal "fetch a URL" helper. It
// supersedes the per-service habit of hand-rolling net/http.Get for HTML
// scraping: instead of every service reimplementing redirect handling,
// encoding sniffing, SSRF guard, Set-Cookie collection and UA rotation,
// callers do:
//
//	res, err := client.Fetch(ctx, url)
//
// and get back a normalised FetchResult regardless of which backend
// actually served the request. The helper picks a backend in this order:
//
//  1. js-proxy   (when WithJSRender(true) is set — full Chromium render,
//     returns rendered DOM + network[] + cookies_set[]).
//     Requires JS_PROXY_URL + JS_PROXY_API_KEY. Fails hard
//     on error; we never silently downgrade to plain HTML
//     when the caller asked for a rendered DOM.
//
//  2. html-proxy (only when HTML_PROXY_URL is explicitly set — a
//     dedicated proxy service that handles
//     redirect/encoding/CF normalisation upstream of this
//     caller. Currently no fleet service ships this role;
//     the slot is reserved for when one does. Falls back
//     to direct on error.)
//
//  3. direct     (the DEFAULT path — uses safehttp.NewClient for SSRF
//     protection plus a CheckRedirect callback that captures
//     each hop's headers + Set-Cookie trail. The
//     "centralisation" lives in this library function, not
//     in a separate service: services that call Fetch all
//     share the same SSRF guard, redirect handler, cookie
//     collector, and UA. JS rendering is unavailable on
//     this path; opt in with WithJSRender if you need it.)
//
// FetchResult.Backend says which path won. FetchResult.Degraded is true
// whenever the helper fell back from the requested backend; the
// degradation reason is in DegradedReason. Callers can surface this via
// depcheck or just log it; the data is always present.
//
// Environment variables (all optional):
//
//	HTML_PROXY_URL          if set, route through this URL before direct
//	HTML_PROXY_API_KEY      key for the html-proxy backend
//	JS_PROXY_URL            default https://go-js-proxy-network.0exec.com
//	JS_PROXY_API_KEY        required when WithJSRender(true)
//	FLEET_API_KEY           single fleet-wide key, used when *_API_KEY unset
//	FETCHPROXY_ALLOW_DIRECT "false" disables the direct-fetch fallback
//
// No keys are baked in: go-common is a public repo and must stay clean.
// Services run in production with real keys injected via .env; for
// in-fleet demos the gateway accepts the literal "default_token", but
// that string is gateway-side, not source-side. See services-registry
// CLAUDE.md "Auth — how mesh-0exec actually authenticates".
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/safehttp"
)

// FetchOption configures a single Fetch call. Options are additive; later
// options override earlier ones if they touch the same field.
type FetchOption func(*fetchConfig)

type fetchConfig struct {
	jsRender    bool
	networkLog  bool
	timeout     time.Duration
	userAgent   string
	allowDirect *bool // nil => env decides
}

// WithJSRender, when true, routes the request to the js-proxy so the
// returned Body is the post-mount DOM (cost: 1-3s extra latency, full
// Chromium render). Default false — plain HTML fetch is much cheaper.
func WithJSRender(b bool) FetchOption {
	return func(c *fetchConfig) { c.jsRender = b }
}

// WithNetworkLog, when true, asks the helper to attach the full
// rendered-page telemetry to the returned FetchResult: Network,
// ConsoleLogs, Performance, and the rich CookiesSet observed by the
// browser. Implies WithJSRender(true) — there is no source of this
// data on the direct or html-proxy paths.
//
// Use this when downstream logic needs to walk the request tree
// (XHR/fetch endpoints, dynamically-loaded JS bundles, redirect
// params observed in real navigation, console errors, etc.). The
// payload is already in memory after a JS render; opt-in is purely
// to make the cost/intent explicit at the call site.
func WithNetworkLog(b bool) FetchOption {
	return func(c *fetchConfig) {
		c.networkLog = b
		if b {
			c.jsRender = true
		}
	}
}

// WithFetchTimeout caps total request budget (default 25s).
func WithFetchTimeout(d time.Duration) FetchOption {
	return func(c *fetchConfig) { c.timeout = d }
}

// WithFetchUserAgent overrides the User-Agent forwarded by the proxy
// (default: whatever the proxy backend supplies).
func WithFetchUserAgent(s string) FetchOption {
	return func(c *fetchConfig) { c.userAgent = s }
}

// WithAllowDirect explicitly opts in or out of the direct safehttp
// fallback when both proxies are unreachable. Overrides
// FETCHPROXY_ALLOW_DIRECT env var. Default (env unset, option unused):
// fallback is enabled — services degrade rather than fail hard.
func WithAllowDirect(b bool) FetchOption {
	return func(c *fetchConfig) { c.allowDirect = &b }
}

// FetchHop is one entry in the redirect chain. SetCookie is the raw
// Set-Cookie header values from THIS hop (one slice element per
// Set-Cookie header on the response); join across hops to get the full
// cookie trail.
type FetchHop struct {
	URL       string            `json:"url"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	SetCookie []string          `json:"set_cookie,omitempty"`
}

// FetchCookie is one cookie observed during the fetch, with the URL
// that issued it and the raw Set-Cookie header for downstream parsing.
type FetchCookie struct {
	Raw    string `json:"raw"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
	SetOn  string `json:"set_on"` // URL that set it
}

// FetchResult is the unified shape returned by Fetch — same struct
// regardless of which backend (html-proxy / js-proxy / direct) served
// the request.
//
// Rich-telemetry fields (Network, ConsoleLogs, Performance) are
// populated only when the caller passes WithNetworkLog(true) (which
// implies a JS render). They are nil on the html-proxy and direct
// paths because those backends do not produce that data. Callers
// should nil-check before walking them; the netextract.go helpers do.
type FetchResult struct {
	URL            string            `json:"url"`            // original request
	FinalURL       string            `json:"final_url"`      // after redirects
	Status         int               `json:"status"`         // final response status
	Headers        map[string]string `json:"headers"`        // final response headers
	Body           string            `json:"body"`           // final response body (rendered DOM when js-proxy)
	RedirectChain  []FetchHop        `json:"redirect_chain"` // all hops INCLUDING final
	CookiesSet     []FetchCookie     `json:"cookies_set"`    // Set-Cookie across all hops
	Backend        string            `json:"backend"`        // "html-proxy"|"js-proxy"|"direct"
	Degraded       bool              `json:"degraded"`       // fell back from requested backend
	DegradedReason string            `json:"degraded_reason,omitempty"`

	// Network is every HTTP request the browser made while rendering
	// the page (document, scripts, XHR, fetch, images, fonts…). Each
	// entry carries URL, method, status, request+response headers,
	// resource_type, initiator and timing. Populated only when
	// WithNetworkLog(true) was passed.
	Network []NetworkEntry `json:"network,omitempty"`

	// ConsoleLogs is the verbatim browser console output (info, warn,
	// error). Most useful: framework dev-mode warnings, stack traces,
	// unhandled-promise rejections. Populated only when
	// WithNetworkLog(true) was passed.
	ConsoleLogs []string `json:"console_logs,omitempty"`

	// Performance carries the CDP lifecycle events and the
	// window.performance.timing snapshot from the rendered page.
	// Populated only when WithNetworkLog(true) was passed.
	Performance *Performance `json:"performance,omitempty"`
}

// HasNetwork reports whether the rich-telemetry fields are present.
// Callers use this as a guard before walking Network/ConsoleLogs/etc.
// — keeps the "did we ask for it?" check at one obvious site instead
// of replicated nil-checks in every consumer.
func (r *FetchResult) HasNetwork() bool {
	return r != nil && r.Network != nil
}

// Defaults & env-key names — exported for test/override use.
const (
	DefaultFetchTimeout = 25 * time.Second
	DefaultBodyCap      = 32 << 20 // 32 MiB per response
)

var fetchHTTPClient = &http.Client{Timeout: DefaultFetchTimeout}

// Fetch retrieves target through the configured proxy chain. See package
// doc for backend selection.
func Fetch(ctx context.Context, target string, opts ...FetchOption) (*FetchResult, error) {
	if target == "" {
		return nil, errors.New("fetchproxy: target is required")
	}
	cfg := fetchConfig{timeout: DefaultFetchTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	allowDirect := os.Getenv("FETCHPROXY_ALLOW_DIRECT") != "false"
	if cfg.allowDirect != nil {
		allowDirect = *cfg.allowDirect
	}

	if cfg.jsRender {
		res, err := fetchViaJSProxy(ctx, target, cfg)
		if err == nil {
			return res, nil
		}
		// JS render is a hard requirement; do NOT silently downgrade to
		// plain HTML or direct fetch — caller asked for rendered DOM.
		return nil, fmt.Errorf("fetchproxy: js-proxy required but failed: %w", err)
	}

	// html-proxy is opt-in: only attempt if HTML_PROXY_URL is explicitly
	// set. The default path is direct safehttp (which is itself the
	// "centralised" fetcher with SSRF guard + redirect tracking).
	if os.Getenv("HTML_PROXY_URL") != "" {
		res, err := fetchViaHTMLProxy(ctx, target, cfg)
		if err == nil {
			return res, nil
		}
		if !allowDirect {
			return nil, fmt.Errorf("fetchproxy: html-proxy failed and direct fallback disabled: %w", err)
		}
		// Fall through to direct, marking degraded.
		dRes, dErr := fetchDirect(ctx, target, cfg)
		if dErr != nil {
			return nil, fmt.Errorf("fetchproxy: html-proxy failed (%v) AND direct fetch failed: %w", err, dErr)
		}
		dRes.Degraded = true
		dRes.DegradedReason = "html-proxy unreachable: " + err.Error()
		return dRes, nil
	}

	if !allowDirect {
		return nil, errors.New("fetchproxy: HTML_PROXY_URL unset and direct fetch disabled")
	}
	return fetchDirect(ctx, target, cfg)
}

// FetchHTMLResult is what html-proxy returns. Matches the JSON shape
// served by go-html-proxy's /fetch endpoint; kept in lockstep with that
// service. See go-html-proxy/main.go for the canonical shape.
type htmlProxyResponse struct {
	URL           string            `json:"url"`
	FinalURL      string            `json:"final_url"`
	Status        int               `json:"status"`
	Headers       map[string]string `json:"headers"`
	Body          string            `json:"body"`
	RedirectChain []FetchHop        `json:"redirect_chain"`
	CookiesSet    []FetchCookie     `json:"cookies_set"`
}

func fetchViaHTMLProxy(ctx context.Context, target string, cfg fetchConfig) (*FetchResult, error) {
	base := os.Getenv("HTML_PROXY_URL") // caller already checked it's non-empty
	apiKey := envFirst("HTML_PROXY_API_KEY", "FLEET_API_KEY")
	if apiKey == "" {
		return nil, errors.New("html-proxy: HTML_PROXY_API_KEY or FLEET_API_KEY required")
	}

	q := url.Values{}
	q.Set("url", target)
	q.Set("api_key", apiKey)
	if cfg.userAgent != "" {
		q.Set("ua", cfg.userAgent)
	}
	reqURL := strings.TrimRight(base, "/") + "/fetch?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("html-proxy: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("html-proxy: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, DefaultBodyCap))
	if err != nil {
		return nil, fmt.Errorf("html-proxy: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("html-proxy: upstream returned %d: %s", resp.StatusCode, string(snippet))
	}

	var raw htmlProxyResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("html-proxy: decode JSON: %w", err)
	}
	return &FetchResult{
		URL:           raw.URL,
		FinalURL:      raw.FinalURL,
		Status:        raw.Status,
		Headers:       raw.Headers,
		Body:          raw.Body,
		RedirectChain: raw.RedirectChain,
		CookiesSet:    raw.CookiesSet,
		Backend:       "html-proxy",
	}, nil
}

func fetchViaJSProxy(ctx context.Context, target string, cfg fetchConfig) (*FetchResult, error) {
	// Pick the right backend by intent:
	//   networkLog requested → go-js-proxy-network (expensive, returns
	//                          DOM + network + console + performance)
	//   plain JS render       → go-js-proxy (cheap, returns just DOM)
	// Picking based on intent — not on env or a single default — means
	// services that only need the rendered DOM don't pay the cost of
	// (or get rate-limited by) the network-aware proxy.
	var pr *ProxyResult
	var err error
	if cfg.networkLog {
		pr, err = JSProxy(ctx, target)
	} else {
		pr, err = JSProxyDOM(ctx, target)
	}
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	var lastEntry *NetworkEntry
	// The final document response is the most recent main-frame network
	// entry whose URL matches FinalURL. Fall back to the last navigation
	// entry if no exact match.
	for i := range pr.Network {
		e := &pr.Network[i]
		if e.URL == pr.FinalURL || strings.HasPrefix(pr.FinalURL, e.URL) {
			lastEntry = e
		}
	}
	status := 0
	if lastEntry != nil {
		for k, v := range lastEntry.ResponseHeaders {
			headers[k] = v
		}
		status = lastEntry.Status
	}

	cookies := make([]FetchCookie, 0, len(pr.CookiesSet))
	for _, m := range pr.CookiesSet {
		cookies = append(cookies, FetchCookie{
			Raw:   m["raw"],
			Name:  m["name"],
			Value: m["value"],
			SetOn: pr.FinalURL,
		})
	}

	// Reconstruct a minimal redirect chain from the navigation entries.
	chain := []FetchHop{}
	for _, e := range pr.Network {
		if e.ResourceType != "document" && e.ResourceType != "" {
			continue
		}
		chain = append(chain, FetchHop{
			URL:     e.URL,
			Status:  e.Status,
			Headers: e.ResponseHeaders,
		})
	}

	res := &FetchResult{
		URL:           target,
		FinalURL:      pr.FinalURL,
		Status:        status,
		Headers:       headers,
		Body:          pr.DOMHTML,
		RedirectChain: chain,
		CookiesSet:    cookies,
		Backend:       "js-proxy",
	}
	if cfg.networkLog {
		res.Network = pr.Network
		res.ConsoleLogs = pr.ConsoleLogs
		perf := pr.Performance
		res.Performance = &perf
	}
	return res, nil
}

func fetchDirect(ctx context.Context, target string, cfg fetchConfig) (*FetchResult, error) {
	// Tracks every hop ourselves because Go's http.Client only exposes
	// the final response.
	hops := []FetchHop{}
	cookies := []FetchCookie{}

	client := safehttp.NewClient(
		safehttp.WithTimeout(cfg.timeout),
		safehttp.WithUserAgent(cfg.userAgent),
	)
	// Wrap CheckRedirect to capture each hop's headers + Set-Cookie.
	prev := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if n := len(via); n > 0 {
			last := via[n-1].Response
			if last != nil {
				hops = append(hops, hopFromResp(last))
				for _, raw := range last.Header.Values("Set-Cookie") {
					if c := parseSetCookie(raw, last.Request.URL.String()); c != nil {
						cookies = append(cookies, *c)
					}
				}
			}
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("direct: build request: %w", err)
	}
	if cfg.userAgent != "" {
		req.Header.Set("User-Agent", cfg.userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("direct: do: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, DefaultBodyCap))
	if err != nil {
		return nil, fmt.Errorf("direct: read body: %w", err)
	}
	hops = append(hops, hopFromResp(resp))
	for _, raw := range resp.Header.Values("Set-Cookie") {
		if c := parseSetCookie(raw, resp.Request.URL.String()); c != nil {
			cookies = append(cookies, *c)
		}
	}
	headers := flatHeaders(resp.Header)

	return &FetchResult{
		URL:           target,
		FinalURL:      resp.Request.URL.String(),
		Status:        resp.StatusCode,
		Headers:       headers,
		Body:          string(body),
		RedirectChain: hops,
		CookiesSet:    cookies,
		Backend:       "direct",
	}, nil
}

func hopFromResp(r *http.Response) FetchHop {
	h := FetchHop{
		URL:     r.Request.URL.String(),
		Status:  r.StatusCode,
		Headers: flatHeaders(r.Header),
	}
	if v := r.Header.Values("Set-Cookie"); len(v) > 0 {
		h.SetCookie = append([]string(nil), v...)
	}
	return h
}

func flatHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vv := range h {
		out[k] = strings.Join(vv, ", ")
	}
	return out
}

// parseSetCookie does the minimum needed for the Result envelope: name,
// value, domain. The raw header is preserved verbatim so downstream
// services (cookie-checker) can re-parse with strict RFC 6265bis rules.
func parseSetCookie(raw, setOn string) *FetchCookie {
	if raw == "" {
		return nil
	}
	c := FetchCookie{Raw: raw, SetOn: setOn}
	parts := strings.Split(raw, ";")
	if len(parts) == 0 {
		return nil
	}
	nv := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
	if len(nv) != 2 {
		return nil
	}
	c.Name = nv[0]
	c.Value = nv[1]
	for _, p := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], "Domain") {
			c.Domain = kv[1]
		}
	}
	return &c
}

// ProbeHTMLProxy is a depcheck-friendly probe for the html-proxy backend.
// Returns nil immediately if HTML_PROXY_URL is unset (no backend to
// probe — services running on the direct path don't need a probe).
// Otherwise hits its /health endpoint with a 2s budget; returns nil if
// 200 OK.
func ProbeHTMLProxy(ctx context.Context) error {
	base := os.Getenv("HTML_PROXY_URL")
	if base == "" {
		return nil
	}
	return probeHealth(ctx, base)
}

// ProbeJSProxy is a depcheck-friendly probe for the network-aware
// js-proxy backend (go-js-proxy-network). Use this when your service
// depends on the network log being available.
func ProbeJSProxy(ctx context.Context) error {
	base := envFirst("JS_PROXY_NETWORK_URL", "JS_PROXY_URL")
	if base == "" {
		base = DefaultJSProxyNetworkURL
	}
	return probeHealth(ctx, base)
}

// ProbeJSProxyDOM is a depcheck-friendly probe for the DOM-only
// js-proxy backend (go-js-proxy). Use this when your service only
// renders pages and never walks the network array — the cheaper
// proxy will still serve you and that's the dep you should declare.
func ProbeJSProxyDOM(ctx context.Context) error {
	base := env.String("JS_PROXY_DOM_URL", DefaultJSProxyDOMURL)
	return probeHealth(ctx, base)
}

func probeHealth(ctx context.Context, base string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reqURL := strings.TrimRight(base, "/") + "/health"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	resp, err := fetchHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", base, resp.StatusCode)
	}
	return nil
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

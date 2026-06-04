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
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Defaults & env-key names — exported for test/override use.
const (
	DefaultFetchTimeout = 25 * time.Second
	DefaultBodyCap      = 32 << 20 // 32 MiB per response
)

var fetchHTTPClient = &http.Client{Timeout: DefaultFetchTimeout}

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

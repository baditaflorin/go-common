// Package client - jsproxy.go provides clients for the fleet's two
// JS-rendering proxies. They are SEPARATE services with separate
// payloads — pick the one that matches your need:
//
//	go-js-proxy           https://go-js-proxy.0exec.com
//	                      Rendered DOM only. Cheaper. Use when all you
//	                      need is the post-mount HTML.
//	                      Function: JSProxyDOM(ctx, target)
//
//	go-js-proxy-network   https://go-js-proxy-network.0exec.com
//	                      Rendered DOM PLUS the full network log
//	                      (every request, response headers, sizes,
//	                      timings) PLUS console output PLUS the
//	                      Performance API snapshot. More expensive,
//	                      rate-limited harder. Use when downstream
//	                      logic needs to walk request/response
//	                      telemetry (XHR endpoints, loaded scripts,
//	                      Set-Cookie chain, etc.).
//	                      Function: JSProxy(ctx, target)
//
// `client.Fetch(ctx, target, ...)` picks the right backend
// automatically based on whether the caller asked for the network
// log: WithJSRender(true) alone routes to the DOM-only proxy;
// WithNetworkLog(true) routes to the network-aware proxy.
//
// Environment:
//
//	JS_PROXY_NETWORK_URL      Network-aware proxy base URL. Default:
//	                          https://go-js-proxy-network.0exec.com
//	JS_PROXY_NETWORK_API_KEY  API key for the network-aware proxy.
//	JS_PROXY_DOM_URL          DOM-only proxy base URL. Default:
//	                          https://go-js-proxy.0exec.com
//	JS_PROXY_DOM_API_KEY      API key for the DOM-only proxy.
//	JS_PROXY_URL              Backward-compat alias for
//	                          JS_PROXY_NETWORK_URL.
//	JS_PROXY_API_KEY          Backward-compat fallback used by either
//	                          proxy if its specific *_API_KEY env var
//	                          is unset. Existed before the split; do
//	                          not rely on it in new code.
//
// The two proxies use SEPARATE keys — one key does not authenticate
// against the other. Set `JS_PROXY_DOM_API_KEY` and
// `JS_PROXY_NETWORK_API_KEY` independently. The `JS_PROXY_API_KEY`
// fallback only exists so deployments wired before the split don't
// break; once both specific keys are set it's ignored.
//
// The helper is intentionally dependency-free: it uses net/http directly so
// it can be vendored in services with strict build constraints.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NetworkEntry is one row of the captured network log for a JS-rendered page.
type NetworkEntry struct {
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Status          int               `json:"status"`
	RequestHeaders  map[string]string `json:"request_headers"`
	ResponseHeaders map[string]string `json:"response_headers"`
	ResponseSize    int64             `json:"response_size"`
	TimingMs        int64             `json:"timing_ms"`
	ResourceType    string            `json:"resource_type"`
	Initiator       string            `json:"initiator"`
}

// Performance groups page-load timings. Two sub-maps because the keyspaces
// collide (e.g. `load` exists in both CDP lifecycle events and the legacy
// window.performance.timing API). Splitting them keeps both losslessly.
type Performance struct {
	// Lifecycle is the set of CDP page-lifecycle events we record:
	// firstPaint, firstContentfulPaint, DOMContentLoaded, load, init.
	// Values are milliseconds since the unix epoch.
	Lifecycle map[string]int64 `json:"lifecycle"`
	// Timing is window.performance.timing.toJSON() verbatim — keys like
	// navigationStart, responseEnd, loadEventEnd, etc.
	Timing map[string]int64 `json:"timing"`
}

// ProxyResult is what JSProxy returns. Field tags match the go-js-proxy-network
// JSON response so the same struct round-trips through both ends.
type ProxyResult struct {
	FinalURL    string              `json:"final_url"`
	DOMHTML     string              `json:"dom_html"`
	Network     []NetworkEntry      `json:"network"`
	ConsoleLogs []string            `json:"console_logs"`
	Performance Performance         `json:"performance"`
	CookiesSet  []map[string]string `json:"cookies_set"`
}

// Defaults — kept exported so callers can override per-call.
const (
	DefaultJSProxyNetworkURL = "https://go-js-proxy-network.0exec.com"
	DefaultJSProxyDOMURL     = "https://go-js-proxy.0exec.com"
	DefaultJSProxyTimeMS     = 20000 // 20s — give the server up to its own 15s hard timeout

	// Deprecated: use DefaultJSProxyNetworkURL.
	DefaultJSProxyURL = DefaultJSProxyNetworkURL
	// Deprecated: use DefaultJSProxyDOMURL.
	LegacyJSProxyURL = DefaultJSProxyDOMURL
)

// jsProxyHTTPClient is shared so we get connection reuse across calls.
var jsProxyHTTPClient = &http.Client{
	Timeout: 25 * time.Second,
}

// JSProxy renders targetURL through the **network-aware** proxy
// (go-js-proxy-network) and returns the parsed result including the
// Network log, ConsoleLogs and Performance fields. Use this when
// downstream logic needs request-tree telemetry. For just the DOM,
// prefer JSProxyDOM — it's cheaper and rate-limited less aggressively.
func JSProxy(ctx context.Context, targetURL string) (*ProxyResult, error) {
	if targetURL == "" {
		return nil, errors.New("jsproxy: targetURL is required")
	}
	apiKey := envFirst("JS_PROXY_NETWORK_API_KEY", "JS_PROXY_API_KEY")
	if apiKey == "" {
		return nil, errors.New("jsproxy: JS_PROXY_NETWORK_API_KEY (or legacy JS_PROXY_API_KEY) env var is required")
	}
	base := envFirst("JS_PROXY_NETWORK_URL", "JS_PROXY_URL")
	if base == "" {
		base = DefaultJSProxyNetworkURL
	}
	return jsProxyNetwork(ctx, base, apiKey, targetURL)
}

// JSProxyDOM renders targetURL through the **DOM-only** proxy
// (go-js-proxy) and returns a ProxyResult populated only with the
// rendered DOM. Network/ConsoleLogs/Performance fields are nil — this
// proxy does not produce them. Cheaper than JSProxy; use it when you
// only need the rendered HTML.
func JSProxyDOM(ctx context.Context, targetURL string) (*ProxyResult, error) {
	if targetURL == "" {
		return nil, errors.New("jsproxy: targetURL is required")
	}
	apiKey := envFirst("JS_PROXY_DOM_API_KEY", "JS_PROXY_API_KEY")
	if apiKey == "" {
		return nil, errors.New("jsproxy: JS_PROXY_DOM_API_KEY (or legacy JS_PROXY_API_KEY) env var is required")
	}
	base := envOr("JS_PROXY_DOM_URL", DefaultJSProxyDOMURL)
	return jsProxyDOM(ctx, base, apiKey, targetURL)
}

func jsProxyNetwork(ctx context.Context, base, apiKey, target string) (*ProxyResult, error) {
	q := url.Values{}
	q.Set("url", target)
	q.Set("api_key", apiKey)

	reqURL := strings.TrimRight(base, "/") + "/?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jsproxy: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := jsProxyHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jsproxy: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64MiB hard cap
	if err != nil {
		return nil, fmt.Errorf("jsproxy: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("jsproxy: upstream returned %d: %s", resp.StatusCode, string(snippet))
	}

	var out ProxyResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("jsproxy: decode JSON: %w", err)
	}
	if out.FinalURL == "" {
		out.FinalURL = target
	}
	return &out, nil
}

// jsProxyDOM speaks to go-js-proxy (the DOM-only service). Returns
// the rendered HTML in DOMHTML; Network/ConsoleLogs/Performance stay
// nil because this proxy does not produce them.
func jsProxyDOM(ctx context.Context, base, apiKey, target string) (*ProxyResult, error) {
	q := url.Values{}
	q.Set("url", target)
	q.Set("api_key", apiKey)
	reqURL := strings.TrimRight(base, "/") + "/?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jsproxy(dom): build request: %w", err)
	}
	resp, err := jsProxyHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jsproxy(dom): do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32MiB cap
	if err != nil {
		return nil, fmt.Errorf("jsproxy(dom): read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("jsproxy(dom): upstream returned %d: %s", resp.StatusCode, string(snippet))
	}

	return &ProxyResult{
		FinalURL:    target,
		DOMHTML:     string(body),
		Network:     nil,
		ConsoleLogs: nil,
		Performance: Performance{},
		CookiesSet:  nil,
	}, nil
}

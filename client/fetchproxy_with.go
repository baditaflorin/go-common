package client

import (
	"time"
)

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

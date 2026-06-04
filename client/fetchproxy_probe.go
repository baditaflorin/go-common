package client

import (
	"context"
	"github.com/baditaflorin/go-common/env"
	"os"
)

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

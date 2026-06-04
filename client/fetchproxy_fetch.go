package client

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// FetchOption configures a single Fetch call. Options are additive; later
// options override earlier ones if they touch the same field.
type FetchOption func(*fetchConfig)

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

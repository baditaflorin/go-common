// Package client - jsproxy.go provides a tiny client for the
// `go-js-proxy-network` service (and its legacy `go-js-proxy` fallback).
//
// Each service in the fleet that needs a JS-rendered page calls
// client.JSProxy(ctx, targetURL) and gets back a parsed ProxyResult — DOM
// HTML plus the full network log, console output, and Performance API
// timings.
//
// Environment:
//
//	JS_PROXY_URL      Base URL of the new network-aware proxy. Default:
//	                  https://go-js-proxy-network.0exec.com
//	JS_PROXY_API_KEY  API key sent as ?api_key=. Required.
//	JS_PROXY_LEGACY   If "true", or if JS_PROXY_URL is empty, falls back
//	                  to the DOM-only https://go-js-proxy.0exec.com and
//	                  synthesises a minimal ProxyResult.
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
	"os"
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

// ProxyResult is what JSProxy returns. Field tags match the go-js-proxy-network
// JSON response so the same struct round-trips through both ends.
type ProxyResult struct {
	FinalURL    string              `json:"final_url"`
	DOMHTML     string              `json:"dom_html"`
	Network     []NetworkEntry      `json:"network"`
	ConsoleLogs []string            `json:"console_logs"`
	Performance map[string]int64    `json:"performance"`
	CookiesSet  []map[string]string `json:"cookies_set"`
}

// Defaults — kept exported so callers can override per-call.
const (
	DefaultJSProxyURL    = "https://go-js-proxy-network.0exec.com"
	LegacyJSProxyURL     = "https://go-js-proxy.0exec.com"
	DefaultJSProxyTimeMS = 20000 // 20s — give the server up to its own 15s hard timeout
)

// jsProxyHTTPClient is shared so we get connection reuse across calls.
var jsProxyHTTPClient = &http.Client{
	Timeout: 25 * time.Second,
}

// legacyJSProxyURL is the base used when JS_PROXY_LEGACY=true. Exposed as a
// var so tests can swap it.
var legacyJSProxyURL = LegacyJSProxyURL

// JSProxy renders targetURL through the configured JS proxy and returns
// the parsed result. It picks the new network-aware proxy by default and
// transparently falls back to the legacy DOM-only proxy if JS_PROXY_URL is
// unset or JS_PROXY_LEGACY=true.
func JSProxy(ctx context.Context, targetURL string) (*ProxyResult, error) {
	if targetURL == "" {
		return nil, errors.New("jsproxy: targetURL is required")
	}

	apiKey := os.Getenv("JS_PROXY_API_KEY")
	if apiKey == "" {
		return nil, errors.New("jsproxy: JS_PROXY_API_KEY env var is required")
	}

	base := os.Getenv("JS_PROXY_URL")
	legacy := strings.EqualFold(os.Getenv("JS_PROXY_LEGACY"), "true")
	if base == "" {
		base = DefaultJSProxyURL
	}
	if legacy {
		return jsProxyLegacy(ctx, legacyJSProxyURL, apiKey, targetURL)
	}

	return jsProxyModern(ctx, base, apiKey, targetURL)
}

func jsProxyModern(ctx context.Context, base, apiKey, target string) (*ProxyResult, error) {
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

// jsProxyLegacy synthesises a ProxyResult from the old DOM-only proxy. Network,
// performance and console fields will be empty — callers that need them must
// upgrade to the modern endpoint.
func jsProxyLegacy(ctx context.Context, base, apiKey, target string) (*ProxyResult, error) {
	q := url.Values{}
	q.Set("url", target)
	q.Set("api_key", apiKey)
	reqURL := strings.TrimRight(base, "/") + "/?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jsproxy(legacy): build request: %w", err)
	}
	resp, err := jsProxyHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jsproxy(legacy): do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32MiB cap
	if err != nil {
		return nil, fmt.Errorf("jsproxy(legacy): read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := body
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("jsproxy(legacy): upstream returned %d: %s", resp.StatusCode, string(snippet))
	}

	return &ProxyResult{
		FinalURL:    target,
		DOMHTML:     string(body),
		Network:     nil,
		ConsoleLogs: nil,
		Performance: nil,
		CookiesSet:  nil,
	}, nil
}

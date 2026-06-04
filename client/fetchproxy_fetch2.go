package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/baditaflorin/go-common/safehttp"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type fetchConfig struct {
	jsRender    bool
	networkLog  bool
	timeout     time.Duration
	userAgent   string
	allowDirect *bool // nil => env decides
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

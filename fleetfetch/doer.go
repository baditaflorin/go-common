package fleetfetch

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Doer is the minimal interface a producer can hold for HTTP fetching.
// Production code holds a *Client; tests can swap in a stub that points
// at an httptest.NewServer-style loopback URL. The migrator emits
// `var fetchClient fleetfetch.Doer = fleetfetch.NewClient(...)` so that
// the test-only assignment compiles.
//
// *Client and *LoopbackClient both satisfy Doer.
type Doer interface {
	Get(ctx context.Context, targetURL string) (*Response, error)
	GetWithMaxAge(ctx context.Context, targetURL string, maxAge time.Duration) (*Response, error)
	GetWithHeaders(ctx context.Context, targetURL string, headers http.Header) (*Response, error)
}

// LoopbackClient is a Doer for unit tests. It wraps a stdlib *http.Client
// (typically `httptest.Server.Client()`) and issues direct GETs — no cache,
// no SSRF guard. Use ONLY in tests. Construct via NewLoopbackClient.
type LoopbackClient struct {
	HTTP *http.Client
}

// NewLoopbackClient returns a Doer that talks directly to URLs via the
// given *http.Client. Pass nil to use http.DefaultClient.
//
// Typical use in handler_test.go:
//
//	srv := httptest.NewServer(...)
//	defer srv.Close()
//	prev := fetchClient
//	fetchClient = fleetfetch.NewLoopbackClient(srv.Client())
//	defer func() { fetchClient = prev }()
//
// This replaces the pre-migration idiom of swapping the safehttp httpClient.
func NewLoopbackClient(h *http.Client) *LoopbackClient {
	if h == nil {
		h = http.DefaultClient
	}
	return &LoopbackClient{HTTP: h}
}

// Get implements Doer.
func (l *LoopbackClient) Get(ctx context.Context, targetURL string) (*Response, error) {
	return l.do(ctx, targetURL, nil)
}

// GetWithMaxAge implements Doer. maxAge is ignored — there is no cache.
func (l *LoopbackClient) GetWithMaxAge(ctx context.Context, targetURL string, _ time.Duration) (*Response, error) {
	return l.do(ctx, targetURL, nil)
}

// GetWithHeaders implements Doer.
func (l *LoopbackClient) GetWithHeaders(ctx context.Context, targetURL string, headers http.Header) (*Response, error) {
	return l.do(ctx, targetURL, headers)
}

func (l *LoopbackClient) do(ctx context.Context, targetURL string, headers http.Header) (*Response, error) {
	if targetURL == "" {
		return nil, errLoopbackEmptyURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	final := targetURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &Response{
		Status:    resp.StatusCode,
		Header:    resp.Header.Clone(),
		Body:      body,
		FinalURL:  final,
		FetchedAt: nowFn(),
	}, nil
}

// Compile-time assertion: *Client and *LoopbackClient both implement Doer.
var _ Doer = (*Client)(nil)
var _ Doer = (*LoopbackClient)(nil)

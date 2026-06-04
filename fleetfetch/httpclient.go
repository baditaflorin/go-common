package fleetfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewHTTPClient returns a standard *http.Client whose RoundTripper
// routes every GET through the fleet fetch cache. Non-GET requests
// fall through to the default transport so the caller can keep using
// the same client for HEAD/POST/PUT without a code path split.
//
// This is the drop-in retrofit shape for services that currently use
// safehttp.NewClient or net/http.Client.Get + .Do(req): swap the
// constructor, keep every other line. resp.Body / resp.StatusCode /
// resp.Header / resp.Request.URL all behave like an ordinary HTTP
// response, but the bytes come from Redis when the URL is already
// cached at the requested render mode.
//
//	client := fleetfetch.NewHTTPClient(
//	    fleetfetch.WithRender(fleetfetch.RenderJS),
//	    fleetfetch.WithTimeout(15*time.Second),
//	)
//	resp, err := client.Get("https://stripe.com")
//	defer resp.Body.Close()
//	body, _ := io.ReadAll(resp.Body)
//
// The returned client has CheckRedirect set to refuse following
// redirects — the cache resolves the final URL upstream and stores
// it as Response.FinalURL, which this adapter surfaces via
// resp.Request.URL. Following them on the consumer side would re-fetch
// without the cache and lose the share.
//
// Per-request headers attached via http.Request.Header are forwarded
// to the upstream fetch using the same X-FF-Forward-* prefix shape
// the cache expects, so the cache key includes them and entries with
// different headers don't conflate.
func NewHTTPClient(opts ...Option) *http.Client {
	c := NewClient(opts...)
	return &http.Client{
		Transport: &fetchCacheTransport{client: c},
		// The outer client timeout MUST exceed one fetch's worst case
		// (a full cache attempt at c.timeout, then — on a slow cache with
		// WithFallbackOnTimeout — a direct fallback fetch also bounded by
		// c.timeout). If it equals c.timeout, the outer deadline cancels
		// the request context at the very moment the cache attempt times
		// out, so directFetch gets an already-dead ctx and the fallback
		// can never run — turning a slow cache into a hard error (the
		// fleet-wide 502s seen on RenderJS migrations, 2026-06-04). The
		// caller's own request context still bounds the real total; this
		// is only the safety backstop, so make it comfortably larger.
		Timeout: 2*c.timeout + 5*time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// fetchCacheTransport is the RoundTripper that intercepts every
// outbound request and routes GETs through the fleet fetch cache.
// Other methods fall through to http.DefaultTransport so the same
// client can be used for the rare non-GET upstream call without a
// per-call branch in user code.
type fetchCacheTransport struct {
	client *Client
}

func (t *fetchCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.Method, http.MethodGet) {
		// Pass non-GETs through the default transport — the cache
		// doesn't accept them anyway, and round-tripping them via
		// fleetfetch would just produce confusing errors.
		return http.DefaultTransport.RoundTrip(req)
	}

	// Forward any caller-set headers as forwarded headers so the cache
	// hashes them into the key and re-attaches them upstream. Drop the
	// hop-by-hop fields http.DefaultTransport would have stripped.
	forward := http.Header{}
	for name, vals := range req.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Proxy-Connection", "Keep-Alive", "Te",
			"Trailer", "Transfer-Encoding", "Upgrade":
			continue
		case "Host", "Content-Length":
			continue
		}
		for _, v := range vals {
			forward.Add(name, v)
		}
	}

	res, err := t.client.fetch(req.Context(), req.URL.String(), 0, forward, t.client.render)
	if err != nil {
		return nil, err
	}

	final := res.FinalURL
	if final == "" {
		final = req.URL.String()
	}
	finalURL, perr := url.Parse(final)
	if perr != nil {
		finalURL = req.URL
	}

	// Surface the cache-side metadata on a clone of the request so
	// callers that do resp.Request.URL.String() see the final URL,
	// matching what they'd see from http.DefaultTransport after a
	// successful redirect chain. Cache headers ride on resp.Header
	// alongside any upstream headers the cache replayed.
	hdr := res.Header.Clone()
	if hdr == nil {
		hdr = http.Header{}
	}
	if res.Render != "" {
		hdr.Set("X-FetchCache-Render", res.Render)
	}
	if res.Via != "" {
		hdr.Set("X-FetchCache-Via", res.Via)
	}
	if res.Hit {
		hdr.Set("X-FetchCache-Hit", "true")
	} else {
		hdr.Set("X-FetchCache-Hit", "false")
	}
	if res.ViaFallback {
		hdr.Set("X-FetchCache-Via-Fallback", "true")
	}

	respReq := req.Clone(req.Context())
	respReq.URL = finalURL

	return &http.Response{
		Status:        fmt.Sprintf("%d %s", res.Status, http.StatusText(res.Status)),
		StatusCode:    res.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Request:       respReq,
	}, nil
}

// SetTimeout updates the underlying *http.Client.Timeout AND the
// fleetfetch client's per-request timeout. Useful when a service
// builds the client once at init and later wants to tighten the
// window for a specific call path.
//
// Goroutine-safety note: this mutates the *http.Client. Don't call
// it concurrently with in-flight requests.
func SetTimeout(httpClient *http.Client, d time.Duration) {
	if httpClient == nil {
		return
	}
	httpClient.Timeout = d
	if t, ok := httpClient.Transport.(*fetchCacheTransport); ok {
		t.client.timeout = d
	}
}

// FromContext is a convenience for handlers that want to ensure the
// outbound request inherits the inbound request's context (for
// cancellation + deadline propagation). Use when calling resp, err :=
// client.Do(req.WithContext(ctx)).
func FromContext(ctx context.Context, req *http.Request) *http.Request {
	if ctx == nil {
		return req
	}
	return req.WithContext(ctx)
}

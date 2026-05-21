package safehttp_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// TestWithUserAgentSetsHeader is the regression test for the silent
// fleet-wide bug where safehttp.NewClient(safehttp.WithUserAgent(...))
// was returning an *http.Client whose Transport did NOT actually
// inject the configured User-Agent on outbound requests. Symptom in
// the wild: Wikidata WDQS (T400119) and other UA-gating upstreams
// returned 403 against Go's default "Go-http-client/1.1" UA, even
// though the service called WithUserAgent. See safehttp/safehttp.go:
// WithUserAgent must wrap a UA-injecting RoundTripper into the chain.
func TestWithUserAgentSetsHeader(t *testing.T) {
	allowLoopback(t)

	var seenUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := safehttp.NewClient(safehttp.WithUserAgent("test-ua/1.0"))
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	got, _ := seenUA.Load().(string)
	if got != "test-ua/1.0" {
		t.Fatalf("server saw User-Agent=%q, want %q (WithUserAgent did not inject the UA into the outbound request)", got, "test-ua/1.0")
	}
}

// TestWithoutUserAgentKeepsGoDefault ensures the fix does not change
// the no-option default. If WithUserAgent is not passed, requests
// continue to go out with Go's default UA ("Go-http-client/...").
// Callers that haven't opted in must see byte-for-byte the same
// outbound shape as pre-fix.
func TestWithoutUserAgentKeepsGoDefault(t *testing.T) {
	allowLoopback(t)

	var seenUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := safehttp.NewClient() // no WithUserAgent
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	got, _ := seenUA.Load().(string)
	if got == "" {
		t.Fatalf("expected Go's default User-Agent, got empty")
	}
	// Go std net/http default UA shape is "Go-http-client/1.1".
	// We don't pin the exact string (Go may evolve it) — just assert
	// safehttp did NOT inject anything of its own.
	if got == "test-ua/1.0" {
		t.Fatalf("UA leaked across tests: %q", got)
	}
}

// TestWithUserAgentDoesNotClobberManualHeader verifies the
// per-request override path. If the caller sets req.Header.Set(
// "User-Agent", ...) before doing the call, that value must win;
// safehttp's UA injection only kicks in when the header is unset.
// This matters for callers that need to spoof a specific UA per
// outbound (scrapers, scanner fingerprinting, A/B tests).
func TestWithUserAgentDoesNotClobberManualHeader(t *testing.T) {
	allowLoopback(t)

	var seenUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := safehttp.NewClient(safehttp.WithUserAgent("client-default/1.0"))
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("newrequest: %v", err)
	}
	req.Header.Set("User-Agent", "per-request-override/2.0")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	got, _ := seenUA.Load().(string)
	if got != "per-request-override/2.0" {
		t.Fatalf("manual UA was clobbered: server saw %q, want %q", got, "per-request-override/2.0")
	}
}

// TestWithUserAgentSurvivesRedirect ensures the injected UA is also
// present on the redirect-follow request. The CheckRedirect hook
// already handled the redirect case (it set the header on the
// follow-up *Request), but the UA-injecting transport must work
// uniformly for both. We assert the upstream sees the configured
// UA on BOTH hops.
func TestWithUserAgentSurvivesRedirect(t *testing.T) {
	allowLoopback(t)

	var (
		uaHop1 atomic.Value
		uaHop2 atomic.Value
	)

	// Final target — captures hop 2.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uaHop2.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// Redirector — captures hop 1 and 302s to target.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uaHop1.Store(r.Header.Get("User-Agent"))
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redir.Close()

	c := safehttp.NewClient(safehttp.WithUserAgent("redirect-ua/1.0"))
	resp, err := c.Get(redir.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	if got, _ := uaHop1.Load().(string); got != "redirect-ua/1.0" {
		t.Errorf("hop1 (pre-redirect) UA = %q, want %q", got, "redirect-ua/1.0")
	}
	if got, _ := uaHop2.Load().(string); got != "redirect-ua/1.0" {
		t.Errorf("hop2 (post-redirect) UA = %q, want %q", got, "redirect-ua/1.0")
	}
}

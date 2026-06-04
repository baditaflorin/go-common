package safehttp

import (
	"crypto/tls"
	"fmt"
	"github.com/baditaflorin/go-common/graph"
	"net/http"
	"net/url"
	"os"
	"time"
)

// NewClient returns an *http.Client with a guarded transport. The transport
// blocks private/internal IPs on every dial, including after redirects, making
// it safe against DNS-rebind and open-redirect SSRF chains.
func NewClient(opts ...Option) *http.Client {
	o := &options{timeout: 8 * time.Second, maxRedirects: 5}
	for _, opt := range opts {
		opt(o)
	}
	if o.withoutProxy && o.requireProxy {
		panic("safehttp: WithoutProxy and RequireProxy are mutually exclusive")
	}
	// Fleet-wide enforcement: FLEET_REQUIRE_PROXY=1 promotes any
	// caller to RequireProxy posture unless they explicitly opted
	// out with WithoutProxy. fleet-runner deploy renders this env
	// var into every service whose service.yaml has proxy_egress:
	// true, so a caller that forgot to pass RequireProxy() still
	// fails-fast on a missing HTTPS_PROXY instead of silently
	// leaking the dockerhost IP.
	if !o.withoutProxy && !o.requireProxy && fleetRequireProxyEnv() {
		o.requireProxy = true
	}
	if o.requireProxy && os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" && os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
		panic("safehttp: RequireProxy set but no HTTP(S)_PROXY env var found — refusing to start (service.yaml has proxy_egress: true but /opt/_shared/proxy.env was not mounted)")
	}
	proxyFn := func(req *http.Request) (*url.URL, error) {
		return proxySkippingPrivate(req)
	}
	if o.withoutProxy {
		proxyFn = nil
	}
	t := newBaseTransport(o, proxyFn)
	// Mirror transport with TLS pinned to ≤ 1.2 — used only as a retry
	// fallback when the default (TLS 1.3) handshake throws an "internal
	// error". Some servers (e.g. older nginx + OpenSSL 3.x combos)
	// negotiate TLS 1.3 ALPN and then send alert 80 mid-handshake; on
	// macOS LibreSSL the same handshake succeeds, so what looks like
	// "site is down" from Linux is actually a server-side TLS quirk.
	// Falling back to 1.2 recovers the response in those cases.
	t12 := t.Clone()
	t12.TLSClientConfig = &tls.Config{MaxVersion: tls.VersionTLS12}

	// Wrap with the fleet-graph observer + TLS-fallback. No-op if
	// GRAPH_ENABLED=false or no collector URL configured. Every outbound
	// call from any fleet service flows through this transport, so this
	// single line gives us fleet-wide outbound observation.
	var rt http.RoundTripper = graph.RoundTripper(&tls12FallbackTransport{primary: t, fallback: t12})

	// If any of the auto-trace / auto-backoff / degraded-sink opt-ins
	// were set, wrap the transport once more so those hooks run on
	// every outbound call. Backwards-compat: with none of the three
	// configured, the chain matches v0.15.0 byte-for-byte.
	// Always wrap with extrasTransport so that a process-wide
	// DefaultObserver installed AFTER NewClient (the canonical
	// server.New → safehttp.SetDefaultObserver flow vs. package-level
	// var clients) is still picked up — extrasTransport.RoundTrip
	// resolves DefaultObserver at CALL time when observer is nil.
	// Pre-v0.26 the wrap was conditional on at least one knob being
	// configured; the unconditional wrap is a no-op when nothing is
	// set (the hot path is one extra function call).
	// Resolve the fetch delegate. A per-client WithFetchDelegate always
	// wins. Otherwise fall back to the process-wide default — but ONLY
	// when the client didn't opt out (WithoutFetchCache) AND isn't a
	// direct-egress client (WithoutProxy). SSRF probers / smuggling
	// detectors / port scanners pass WithoutProxy precisely because they
	// must observe real origin behavior; routing them through the cache
	// would defeat the purpose and hide the origin's true response.
	// The process-wide default delegate is consulted AT CALL TIME (see
	// extrasTransport.RoundTrip) so it reaches clients built before
	// server.New installs it. Here we only capture the per-client explicit
	// delegate and whether this client is eligible to consult the default.
	useDefaultFetchCache := !o.noFetchCache && !o.withoutProxy

	extras := &extrasTransport{
		inner:                rt,
		traceURL:             o.traceURL,
		backoffURL:           o.backoffURL,
		degradedSink:         o.degradedSink,
		observer:             o.observer,
		proxyFn:              proxyFn,
		caller:               callerFromUA(o.userAgent),
		fetchDelegate:        o.fetchDelegate,
		useDefaultFetchCache: useDefaultFetchCache,
	}
	rt = extras

	// User-Agent injection — wraps the transport so EVERY outbound
	// request carries the configured UA. Pre-v0.35.0 the WithUserAgent
	// option only set the UA on redirect-follow requests via the
	// Client.CheckRedirect hook, so the INITIAL request still went
	// out with Go's default "Go-http-client/1.1". Upstreams that gate
	// on UA (Wikidata WDQS T400119, GitHub, many CDNs) silently 403'd
	// every fleet caller. The injection is no-op when WithUserAgent
	// is not set, preserving v0.15.0 behavior for opted-out callers.
	//
	// Per-request override path: if the caller explicitly sets a
	// User-Agent header on the *Request before sending, that value
	// wins — we only inject when the header is unset. This matters
	// for scrapers / fingerprinters that need per-call UA control.
	if o.userAgent != "" {
		rt = &uaTransport{inner: rt, ua: o.userAgent}
	}

	// Egress allowlist — runs as the outermost wrapper so the check
	// fires before any other transport (including extras' backoff
	// consult and the underlying dialer's SSRF guard) attempts I/O.
	// Nil egressAllowlist = no enforcement; matches v0.15.0 chain.
	if o.egressAllowlist != nil {
		rt = &egressAllowlistTransport{
			inner: rt,
			allow: o.egressAllowlist,
		}
	}
	ua, maxR := o.userAgent, o.maxRedirects
	client := &http.Client{
		Transport: rt,
		Timeout:   o.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxR {
				return fmt.Errorf("stopped after %d redirects", maxR)
			}
			if err := ValidateURL(req.URL); err != nil {
				return err
			}
			if ua != "" {
				req.Header.Set("User-Agent", ua)
			}
			return nil
		},
	}
	if o.breakerState != nil && extras != nil {
		store := newBreakerStore(o.breakerState, extras)
		registerBreakerStore(client, store)
	}
	return client
}

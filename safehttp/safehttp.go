package safehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/baditaflorin/go-common/graph"
)

// tls12FallbackTransport tries the primary (TLS 1.3-default) transport
// first; if the response is a "tls: internal error" style failure, it
// retries with a TLS 1.2-capped transport. This recovers from a real-
// world bug class where servers ALPN-negotiate HTTP/2 on TLS 1.3 but
// then send TLS alert 80 ("internal error") mid-handshake. Idempotent
// — only retries on GET-shaped failures where the request body has
// not been read.
type tls12FallbackTransport struct {
	primary  *http.Transport
	fallback *http.Transport
}

func (t *tls12FallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.primary.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isTLSInternalAlert(err) {
		return resp, err
	}
	// Only safe to retry if the body is replayable. For GET/HEAD this
	// is always true. For POST/PUT we'd need GetBody — bail in that case.
	if req.Body != nil && req.GetBody == nil {
		return resp, err
	}
	if req.GetBody != nil {
		nb, gerr := req.GetBody()
		if gerr != nil {
			return resp, err
		}
		req.Body = nb
	}
	return t.fallback.RoundTrip(req)
}

// isTLSInternalAlert matches the TLS alert 80 ("internal error") that
// some servers send when TLS 1.3 + HTTP/2 ALPN goes sideways. It is
// distinct from "tls: handshake failure" and other failures we should
// NOT retry on (cert mismatch, unknown CA, etc.).
func isTLSInternalAlert(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "tls: internal error") ||
		strings.Contains(s, "remote error: tls: internal error")
}

var (
	// ErrBlocked is returned when a target resolves to a non-public network.
	ErrBlocked = errors.New("blocked address: resolves to a non-public network")
	// ErrInvalidScheme is returned for non-http(s) URLs.
	ErrInvalidScheme = errors.New("scheme must be http or https")
	// ErrMissingHost is returned when a URL has no host component.
	ErrMissingHost = errors.New("missing host")
	// ErrEgressNotAllowed is returned when a request targets a host that
	// is not in the configured outbound allowlist (see WithEgressAllowlist
	// / WithDenyAllEgress). The check fires AFTER the SSRF guard but
	// BEFORE any network I/O — no DNS resolution, no TCP connect.
	ErrEgressNotAllowed = errors.New("egress not allowed")
)

// IsBlocked reports whether ip falls in any blocked range:
// loopback, unspecified, multicast, link-local, private (RFC1918),
// CGNAT (100.64.0.0/10), 0.0.0.0/8, or ULA (fc00::/7).
//
// Honors SAFEHTTP_ALLOW_PRIVATE_IPS — a comma-separated list of literal
// IPs that bypass the private-network check. Use it ONLY for trusted
// fleet egress targets (e.g. a LAN-IP loopback past the public NAT
// hairpin); leaving it unset keeps the SSRF defense intact.
func IsBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if isAllowedPrivateIP(ip) {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 {
			return true
		}
		// CGNAT 100.64.0.0/10
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return true
		}
		return false
	}
	// ULA fc00::/7
	if len(ip) == 16 && ip[0]&0xfe == 0xfc {
		return true
	}
	return false
}

// allowedPrivateIPs holds the once-parsed SAFEHTTP_ALLOW_PRIVATE_IPS list.
// Mutable via SetAllowedPrivateIPs (mainly for tests). The default load
// happens at package init from the env var.
var (
	allowedPrivateIPsMu sync.RWMutex
	allowedPrivateIPs   = parseAllowedPrivateIPs(os.Getenv("SAFEHTTP_ALLOW_PRIVATE_IPS"))
)

func parseAllowedPrivateIPs(s string) []net.IP {
	if s == "" {
		return nil
	}
	out := make([]net.IP, 0, 4)
	for _, part := range strings.Split(s, ",") {
		if ip := net.ParseIP(strings.TrimSpace(part)); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func isAllowedPrivateIP(ip net.IP) bool {
	allowedPrivateIPsMu.RLock()
	defer allowedPrivateIPsMu.RUnlock()
	for _, allowed := range allowedPrivateIPs {
		if allowed.Equal(ip) {
			return true
		}
	}
	return false
}

// SetAllowedPrivateIPs replaces the allowlist at runtime. Production
// callers should prefer the env-var path so operators can add a new
// fleet IP without rebuilding the binary.
func SetAllowedPrivateIPs(ips []net.IP) {
	allowedPrivateIPsMu.Lock()
	defer allowedPrivateIPsMu.Unlock()
	cpy := make([]net.IP, len(ips))
	copy(cpy, ips)
	allowedPrivateIPs = cpy
}

// GuardHost resolves host and returns ErrBlocked if any returned IP is in a
// blocked range. Accepts a bare IP literal as well as a hostname. DNS is
// revalidated with a 3-second timeout (DNS-rebind safe when combined with
// the Dialer.Control re-check in NewClient).
func GuardHost(ctx context.Context, host string) error {
	if host == "" {
		return ErrMissingHost
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlocked(ip) {
			return ErrBlocked
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip", host)
	if err != nil {
		return fmt.Errorf("dns lookup failed: %w", err)
	}
	if len(ips) == 0 {
		return ErrBlocked
	}
	for _, ip := range ips {
		if IsBlocked(ip) {
			return ErrBlocked
		}
	}
	return nil
}

// ValidateURL checks that u uses http or https and has a non-empty host.
func ValidateURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidScheme
	}
	if u.Host == "" {
		return ErrMissingHost
	}
	return nil
}

// GuardedDialer returns a DialContext func that blocks connections to private
// or internal addresses. Set portCheck=true to restrict to ports 80 and 443.
// The guard runs before DNS resolution and again at socket creation time
// (Dialer.Control) to defend against DNS-rebind attacks.
func GuardedDialer(portCheck bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return makeDialer(portCheck)
}

// options holds NewClient configuration.
type options struct {
	timeout      time.Duration
	maxRedirects int
	userAgent    string
	portCheck    bool

	// forceHTTP2 sets Transport.ForceAttemptHTTP2 so the client offers
	// "h2" in the TLS ClientHello ALPN. Off by default — see
	// WithForceHTTP2 for why a custom dialer otherwise disables it.
	forceHTTP2 bool

	// Fleet integration hooks — see extras.go for the constructors
	// (WithTraceCollector, WithBackoffCoordinator, WithDegradedSink).
	// All three are opt-in; zero values preserve v0.15.0 behaviour.
	traceURL     string
	backoffURL   string
	degradedSink *[]string

	// Proxy posture — see WithoutProxy / RequireProxy. Defaults to
	// "follow HTTPS_PROXY env if set, else direct" (Go std behaviour).
	withoutProxy bool
	requireProxy bool

	// Egress observer — see WithObserver. nil = no observation.
	observer EgressObserver

	// fetchDelegate, when set, routes eligible outbound GETs through an
	// alternate fetcher (e.g. the fleet fetch cache). See
	// WithFetchDelegate. A per-client delegate wins over the process-wide
	// DefaultFetchDelegate and applies even to withoutProxy clients.
	fetchDelegate FetchDelegate

	// noFetchCache forces the direct egress path even when a process-wide
	// DefaultFetchDelegate is installed. See WithoutFetchCache. An
	// explicit fetchDelegate still wins over this flag.
	noFetchCache bool

	// egressAllowlist, when non-nil, restricts outbound requests to
	// the listed hostnames (lowercased). nil means "no enforcement"
	// — backwards-compatible default. An empty non-nil map means
	// "deny all" (see WithDenyAllEgress). See WithEgressAllowlist.
	egressAllowlist map[string]struct{}

	// Persistent local breaker-state cache — opt-in via
	// WithPersistentBreakerState (see breaker_state.go). Only takes
	// effect when extrasTransport is already in the chain (i.e. at
	// least one of traceURL/backoffURL/degradedSink is set), since
	// the state to persist lives on that transport.
	breakerState *breakerStateConfig
}

// Option configures NewClient.
type Option func(*options)

// WithTimeout sets the total request timeout (default 8s).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithMaxRedirects sets the redirect limit (default 5).
func WithMaxRedirects(n int) Option { return func(o *options) { o.maxRedirects = n } }

// WithUserAgent sets the User-Agent on all requests including redirects.
func WithUserAgent(ua string) Option { return func(o *options) { o.userAgent = ua } }

// WithPortCheck restricts outbound connections to ports 80 and 443 only.
func WithPortCheck() Option { return func(o *options) { o.portCheck = true } }

// WithForceHTTP2 sets Transport.ForceAttemptHTTP2 so the client offers
// "h2" in the TLS ClientHello ALPN and transparently upgrades to HTTP/2
// against capable origins. Read the negotiated protocol back with
// NegotiatedProtocol(resp).
//
// Why it is not the default: NewClient installs a custom DialContext
// (the SSRF guard). Per net/http semantics, providing a custom dialer
// or TLS config "conservatively disables HTTP/2" unless ForceAttemptHTTP2
// is set. So by default the transport offers no "h2" in ALPN and
// resp.TLS.NegotiatedProtocol comes back empty even for HTTP/2-capable
// servers — most visibly on HEAD requests, where there is no body whose
// transfer would otherwise reveal the protocol. Services that need the
// negotiated ALPN (e.g. telling a modern HTTP/2 origin apart from a
// legacy HTTP/1.1-only one) previously worked around this with a
// dedicated TCP/443 TLS handshake; WithForceHTTP2 + NegotiatedProtocol
// is the fleet-canonical replacement.
//
// HTTP/2 still rides the same SSRF-guarded dialer and the TLS-1.2
// fallback transport, so the SSRF and handshake-recovery guarantees are
// unchanged.
func WithForceHTTP2() Option { return func(o *options) { o.forceHTTP2 = true } }

// WithoutProxy explicitly disables proxy use even if HTTP_PROXY /
// HTTPS_PROXY are set in the environment. Use for services that
// legitimately need a direct egress path (SSRF probers, smuggling
// tests, port scanners) where routing through the fleet proxy would
// defeat the purpose. Mutually exclusive with RequireProxy — passing
// both panics at NewClient time.
func WithoutProxy() Option { return func(o *options) { o.withoutProxy = true } }

// RequireProxy asserts HTTPS_PROXY (or HTTP_PROXY) is set in the
// environment at NewClient time. Services declared `proxy_egress:
// true` in service.yaml should pass this so a misconfigured deploy
// fails loudly at startup instead of silently leaking the dockerhost
// IP. Mutually exclusive with WithoutProxy.
//
// Fleet-wide enforcement: when the environment variable
// FLEET_REQUIRE_PROXY=1 is set (rendered by fleet-runner deploy for
// every service with proxy_egress: true), NewClient behaves as if
// RequireProxy() was passed even when the caller forgot to. This is
// the belt-and-suspenders gate that catches every caller that grabs
// safehttp.NewClient() without thinking about proxy posture — see
// the 2026-05-21 Hetzner abuse complaint where five fleet tools
// leaked from the dockerhost IP because their NewClient() calls
// omitted RequireProxy() and the HTTPS_PROXY env happened to be
// unset at startup. WithoutProxy() still wins so SSRF probers,
// smuggling tests, and port scanners can opt out explicitly.
func RequireProxy() Option { return func(o *options) { o.requireProxy = true } }

// fleetRequireProxyEnv returns true when FLEET_REQUIRE_PROXY is set
// to a truthy value (1/true/yes, case-insensitive). Rendered into
// per-service env by fleet-runner deploy when service.yaml has
// proxy_egress: true.
func fleetRequireProxyEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FLEET_REQUIRE_PROXY")))
	return v == "1" || v == "true" || v == "yes"
}

// WithEgressAllowlist restricts outbound requests to the given hostnames.
// Any GET/POST/etc. whose URL.Hostname() is not in the list returns
// ErrEgressNotAllowed without making a network call.
//
// Pass hostnames as exact matches: "api.hetzner.cloud",
// "api.github.com". Wildcards are NOT supported — keep the rule literal
// to force operators to register each fan-out destination explicitly
// (typically sourced from service.yaml auth.calls_external).
//
// Matching is case-insensitive (DNS hostnames are case-insensitive per
// RFC 1035) and ignores the URL port — only the bare host is compared.
//
// Not calling WithEgressAllowlist (the default) means "no allowlist
// enforcement" — existing safehttp behavior is unchanged. To explicitly
// forbid all outbound calls, use WithDenyAllEgress.
//
// The check runs AFTER the existing SSRF guard (private-IP blocking)
// but BEFORE any network I/O is performed, so a blocked call resolves
// no DNS and opens no socket.
func WithEgressAllowlist(hosts ...string) Option {
	return func(o *options) {
		o.egressAllowlist = make(map[string]struct{}, len(hosts))
		for _, h := range hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			o.egressAllowlist[h] = struct{}{}
		}
	}
}

// WithDenyAllEgress is sugar for WithEgressAllowlist with no entries —
// every outbound call returns ErrEgressNotAllowed. Intended for
// services that MUST NOT make external calls (e.g. selftest-only
// fixtures, sandbox harnesses).
func WithDenyAllEgress() Option {
	return func(o *options) {
		o.egressAllowlist = map[string]struct{}{}
	}
}

// proxySkipCache remembers which hostnames resolve to private addresses so
// the DNS lookup only happens once per unique host across all clients.
var (
	proxySkipCacheMu sync.RWMutex
	proxySkipCache   = map[string]bool{}
)

// proxySkippingPrivate wraps http.ProxyFromEnvironment and returns nil (direct)
// when the target host resolves entirely to private / loopback addresses.
// This is needed because Go's standard NO_PROXY CIDR matching only fires for
// IP literals in the URL — Docker compose service names (e.g. go_foo-app-1)
// do not match RFC 1918 CIDR entries even though they resolve to Docker-
// internal addresses. Without this, proxy-egress services route intra-Docker
// traffic through the external Webshare proxy, which cannot resolve those
// names → target_connect_resolve_failed.
func proxySkippingPrivate(req *http.Request) (*url.URL, error) {
	host := req.URL.Hostname()

	// IP literal fast-path.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return nil, nil
		}
		return http.ProxyFromEnvironment(req)
	}

	// Well-known Docker-internal names that may not resolve on Linux.
	if strings.EqualFold(host, "host.docker.internal") || strings.EqualFold(host, "localhost") {
		return nil, nil
	}

	// Cache lookup.
	proxySkipCacheMu.RLock()
	skip, cached := proxySkipCache[host]
	proxySkipCacheMu.RUnlock()
	if cached {
		if skip {
			return nil, nil
		}
		return http.ProxyFromEnvironment(req)
	}

	// Resolve and check whether all addresses are private.
	addrs, err := net.DefaultResolver.LookupHost(req.Context(), host)
	if err == nil && len(addrs) > 0 {
		skip = true
		for _, addr := range addrs {
			ip := net.ParseIP(addr)
			if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()) {
				skip = false
				break
			}
		}
		proxySkipCacheMu.Lock()
		proxySkipCache[host] = skip
		proxySkipCacheMu.Unlock()
	}

	if skip {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

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
	delegate := o.fetchDelegate
	if delegate == nil && !o.noFetchCache && !o.withoutProxy {
		delegate = DefaultFetchDelegate()
	}

	extras := &extrasTransport{
		inner:         rt,
		traceURL:      o.traceURL,
		backoffURL:    o.backoffURL,
		degradedSink:  o.degradedSink,
		observer:      o.observer,
		proxyFn:       proxyFn,
		caller:        callerFromUA(o.userAgent),
		fetchDelegate: delegate,
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

// newBaseTransport builds the primary guarded *http.Transport that
// NewClient wraps. Extracted so the ALPN/HTTP2 behaviour can be
// exercised directly in tests (the wrapped chain hides the underlying
// transport behind unexported types).
func newBaseTransport(o *options, proxyFn func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		// Honor the standard HTTP_PROXY / HTTPS_PROXY / NO_PROXY env vars.
		// This is what every Go program on the planet expects out of an
		// http.Transport; the previous absence meant `safehttp` clients
		// silently bypassed every operator-configured egress proxy. In
		// practice the fleet uses this to route active scans through
		// Webshare residential proxies — direct egress from the dockerhost
		// IP gets German-DC abuse complaints when scanners hit bug-bounty
		// targets.
		//
		// SSRF interaction: when a proxy is in use, the dialer is called
		// with the proxy's IP, not the target's. The target hostname is
		// never resolved by this side. The SSRF guard still runs against
		// the proxy IP (sanity-checks it's a real public host); the
		// target-side guarantees move to the proxy operator. This is the
		// correct trade — explicit env-var opt-in, no surprise behavior.
		//
		// Override via WithoutProxy (force direct) or RequireProxy
		// (fail-fast if env not set). See those options above.
		Proxy:       proxyFn,
		DialContext: makeDialer(o.portCheck),
		// ForceAttemptHTTP2 must be set explicitly: because DialContext
		// above is a custom dialer, net/http otherwise disables HTTP/2,
		// suppressing "h2" in the ALPN offer. Off by default; opt in via
		// WithForceHTTP2. See that option for the full rationale.
		ForceAttemptHTTP2:     o.forceHTTP2,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	}
}

// NegotiatedProtocol returns the TLS ALPN protocol the server selected
// for resp (e.g. "h2" or "http/1.1"), or "" when resp carried no TLS
// state (plain HTTP, or a nil/errored response). It is nil-safe.
//
// To get a reliable "h2" here the client MUST be built with
// WithForceHTTP2 — the default safehttp transport installs a custom
// dialer, which makes net/http conservatively disable HTTP/2 and omit
// "h2" from the ClientHello ALPN, so this field comes back empty even
// against HTTP/2-capable origins. WithForceHTTP2 + NegotiatedProtocol is
// the fleet-canonical replacement for a dedicated TCP/443 ALPN-probe
// handshake.
func NegotiatedProtocol(resp *http.Response) string {
	if resp == nil || resp.TLS == nil {
		return ""
	}
	return resp.TLS.NegotiatedProtocol
}

func makeDialer(portCheck bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if portCheck && port != "80" && port != "443" {
			return nil, fmt.Errorf("blocked port %s: only ports 80 and 443 are allowed", port)
		}
		if err := GuardHost(ctx, host); err != nil {
			return nil, err
		}
		d := &net.Dialer{
			Timeout:   4 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, c syscall.RawConn) error {
				h, _, _ := net.SplitHostPort(address)
				ip := net.ParseIP(h)
				if ip == nil || IsBlocked(ip) {
					return ErrBlocked
				}
				return nil
			},
		}
		return d.DialContext(ctx, network, addr)
	}
}

// NormalizeURL parses raw into a validated *url.URL.
// Prepends "https://" if no scheme is present. Trims whitespace.
func NormalizeURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty url")
	}
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "//") {
			return nil, ErrInvalidScheme
		}
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	return u, ValidateURL(u)
}

// CheckURL normalizes rawURL, validates scheme, and guards the host against
// RFC1918/loopback/CGNAT/ULA addresses. It is the one-call replacement for
// the parse ��� ValidateURL ��� GuardHost pattern used in every service handler.
// Returns the validated, ready-to-use *url.URL.
func CheckURL(ctx context.Context, rawURL string) (*url.URL, error) {
	u, err := NormalizeURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := ValidateURL(u); err != nil {
		return nil, err
	}
	if err := GuardHost(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return u, nil
}

// TestBlockedHosts is a canonical list of hosts/IPs that every service's SSRF
// guard must reject. Use in table-driven tests instead of copying the list.
var TestBlockedHosts = []string{
	"127.0.0.1",
	"::1",
	"10.0.0.1",
	"172.16.0.1",
	"192.168.1.1",
	"169.254.169.254",
	"fc00::1",
	"100.64.0.1",
}

// TestAllowedHosts is a canonical list of public IPs that must NOT be blocked.
var TestAllowedHosts = []string{
	"8.8.8.8",
	"1.1.1.1",
	"93.184.216.34",
}

// uaTransport injects the configured User-Agent on outbound requests
// that don't already have one set. The earlier shape — relying solely
// on Client.CheckRedirect to set the header — only covered redirect-
// follow requests, so the INITIAL request always went out with Go's
// default "Go-http-client/1.1". That was the silent bug fixed in
// v0.35.0: any fleet caller passing WithUserAgent was being 403'd by
// UA-gating upstreams (Wikidata WDQS T400119 was the canary).
//
// Per-request override semantics: we only inject when the header is
// unset (req.Header.Get returns "" for missing). Callers that set a
// per-request UA via req.Header.Set keep that value — required for
// scrapers / scanners that fingerprint per-call.
//
// Request mutation is via req.Clone — http.RoundTripper contracts
// forbid modifying the caller's *Request, and a shared *Request can
// race with the goroutine that constructed it.
type uaTransport struct {
	inner http.RoundTripper
	ua    string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ua != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.inner.RoundTrip(req)
}

// egressAllowlistTransport rejects outbound requests whose target host
// is not in the configured allowlist. The rejection happens BEFORE any
// inner transport (including DNS, dialing, or TLS) is invoked, so a
// blocked call never hits the wire. See WithEgressAllowlist.
type egressAllowlistTransport struct {
	inner http.RoundTripper
	allow map[string]struct{}
}

func (t *egressAllowlistTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rawHost := req.URL.Hostname()
	// Preserve SSRF-guard ordering: if the target is a literal IP in
	// a blocked range, return ErrBlocked first so operators see the
	// stronger signal (SSRF attempt) rather than a generic
	// "not allowed" message. DNS-resolved hostnames still go through
	// the dialer's SSRF guard inside inner.RoundTrip.
	if ip := net.ParseIP(rawHost); ip != nil && IsBlocked(ip) {
		return nil, ErrBlocked
	}
	host := strings.ToLower(rawHost)
	if _, ok := t.allow[host]; !ok {
		return nil, fmt.Errorf("safehttp: %w: %s", ErrEgressNotAllowed, rawHost)
	}
	return t.inner.RoundTrip(req)
}

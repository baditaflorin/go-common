package safehttp

import (
	"context"
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
)

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

	// maxIdleConnsPerHost caps Transport.MaxIdleConnsPerHost. The Go
	// standard library defaults this to 2 (http.DefaultMaxIdleConnsPerHost),
	// which throttles connection reuse for services that hammer a single
	// upstream — every request past the 2nd in flight opens a fresh TCP+TLS
	// connection instead of reusing a pooled one. Zero value means "use the
	// safehttp default" (see defaultMaxIdleConnsPerHost); set explicitly via
	// WithMaxIdleConnsPerHost.
	maxIdleConnsPerHost int

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

// WithMaxIdleConnsPerHost sets Transport.MaxIdleConnsPerHost — the number
// of idle keep-alive connections safehttp will pool per upstream host.
//
// The Go standard library defaults this to 2, which silently caps
// connection reuse: a service issuing many concurrent requests to the same
// host opens (and tears down) a fresh TCP+TLS connection for everything
// past the 2nd in-flight request. safehttp raises the default to 10
// (defaultMaxIdleConnsPerHost). Raise it further for hot single-upstream
// callers (e.g. a tight crawl/poll loop against one API); n is clamped
// below MaxIdleConns (20) by the transport regardless. A value <= 0
// restores the safehttp default.
func WithMaxIdleConnsPerHost(n int) Option {
	return func(o *options) { o.maxIdleConnsPerHost = n }
}

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
		MaxIdleConnsPerHost:   resolveMaxIdleConnsPerHost(o.maxIdleConnsPerHost),
		IdleConnTimeout:       30 * time.Second,
	}
}

// defaultMaxIdleConnsPerHost is safehttp's default cap on idle keep-alive
// connections per upstream host. The Go std default is 2
// (http.DefaultMaxIdleConnsPerHost); we raise it to 10 so high-throughput
// single-upstream callers (the common fleet shape — a service polling one
// API or one sibling) actually reuse pooled connections instead of churning
// TCP+TLS handshakes. Capped below MaxIdleConns (20) so the per-host pool
// can never starve a multi-host client.
const defaultMaxIdleConnsPerHost = 10

// resolveMaxIdleConnsPerHost maps the zero value (caller didn't set the
// option) onto defaultMaxIdleConnsPerHost while letting an explicit value
// through unchanged. A negative value falls back to the default too.
func resolveMaxIdleConnsPerHost(n int) int {
	if n <= 0 {
		return defaultMaxIdleConnsPerHost
	}
	return n
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

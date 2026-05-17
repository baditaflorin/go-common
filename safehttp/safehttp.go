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

	// Fleet integration hooks — see extras.go for the constructors
	// (WithTraceCollector, WithBackoffCoordinator, WithDegradedSink).
	// All three are opt-in; zero values preserve v0.15.0 behaviour.
	traceURL     string
	backoffURL   string
	degradedSink *[]string
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

// NewClient returns an *http.Client with a guarded transport. The transport
// blocks private/internal IPs on every dial, including after redirects, making
// it safe against DNS-rebind and open-redirect SSRF chains.
func NewClient(opts ...Option) *http.Client {
	o := &options{timeout: 8 * time.Second, maxRedirects: 5}
	for _, opt := range opts {
		opt(o)
	}
	t := &http.Transport{
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
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           makeDialer(o.portCheck),
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	}
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
	if o.traceURL != "" || o.backoffURL != "" || o.degradedSink != nil {
		rt = &extrasTransport{
			inner:        rt,
			traceURL:     o.traceURL,
			backoffURL:   o.backoffURL,
			degradedSink: o.degradedSink,
			caller:       callerFromUA(o.userAgent),
		}
	}
	ua, maxR := o.userAgent, o.maxRedirects
	return &http.Client{
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

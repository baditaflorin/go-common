package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

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
func IsBlocked(ip net.IP) bool {
	if ip == nil {
		return true
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
		DialContext:           makeDialer(o.portCheck),
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	}
	ua, maxR := o.userAgent, o.maxRedirects
	return &http.Client{
		Transport: t,
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

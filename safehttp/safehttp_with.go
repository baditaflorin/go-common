package safehttp

import (
	"strings"
	"time"
)

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

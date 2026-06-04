package safehttp

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

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

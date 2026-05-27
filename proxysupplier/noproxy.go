package proxysupplier

import (
	"net"
	"strings"
)

// noProxyRules holds parsed NO_PROXY match rules.
//
// Three rule shapes are supported (Go-stdlib-compatible + a few extensions):
//   - exact:  "localhost", "host.docker.internal"  — equality on the hostname
//   - suffix: ".0crawl.com", "0crawl.com"          — domain suffix (with or without leading dot)
//   - CIDR:   "10.0.0.0/8", "172.16.0.0/12"        — IP/CIDR match
//   - wild:   "*"                                  — match all (disables proxy entirely)
type noProxyRules struct {
	wild   bool
	exact  []string
	suffix []string
	cidrs  []*net.IPNet
}

// parseNoProxy turns a NO_PROXY string into matchers.
// Empty input returns nil (no bypassing).
func parseNoProxy(s string) *noProxyRules {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	r := &noProxyRules{}
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if entry == "*" {
			r.wild = true
			continue
		}
		// CIDR?
		if strings.Contains(entry, "/") {
			if _, ipnet, err := net.ParseCIDR(entry); err == nil {
				r.cidrs = append(r.cidrs, ipnet)
				continue
			}
			// fall through — treat as a literal if CIDR parse failed
		}
		// Literal IP?
		if ip := net.ParseIP(entry); ip != nil {
			r.exact = append(r.exact, ip.String())
			continue
		}
		// Domain suffix? (leading dot OR contains a dot)
		if strings.HasPrefix(entry, ".") {
			r.suffix = append(r.suffix, strings.ToLower(strings.TrimPrefix(entry, ".")))
			continue
		}
		// Heuristic: bare names like "localhost" / "host.docker.internal" are
		// exact matches. Multi-label names like "0crawl.com" without a leading
		// dot are treated as BOTH exact and suffix (matches the domain itself
		// AND its subdomains) — that mirrors Go's stdlib behavior.
		r.exact = append(r.exact, strings.ToLower(entry))
		if strings.Contains(entry, ".") {
			r.suffix = append(r.suffix, strings.ToLower(entry))
		}
	}
	if r.wild || len(r.exact)+len(r.suffix)+len(r.cidrs) > 0 {
		return r
	}
	return nil
}

// Match reports whether the given hostname should bypass the proxy.
//
// host may be an IP literal or a DNS name. CIDR matching first attempts to
// parse host as an IP literal; we deliberately do NOT resolve DNS here
// (would add ~50ms per request and reverse-DNS isn't trustworthy).
func (r *noProxyRules) Match(host string) bool {
	if r == nil {
		return false
	}
	if r.wild {
		return true
	}
	if host == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	// Exact match.
	for _, e := range r.exact {
		if host == e {
			return true
		}
	}

	// CIDR match against IP literal (no DNS resolution).
	if ip := net.ParseIP(host); ip != nil {
		for _, cidr := range r.cidrs {
			if cidr.Contains(ip) {
				return true
			}
		}
	}

	// Suffix match.
	for _, suf := range r.suffix {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}

	return false
}

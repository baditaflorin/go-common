package safehttp

import (
	"errors"
	"sort"
	"strings"
)

// ErrDomainDenied is returned when a target hostname matches the
// fleet-wide compliance denylist (see deniedDomains). It is distinct
// from ErrBlocked (private-network/SSRF) and ErrEgressNotAllowed
// (per-client opt-in allowlist) so callers can surface a specific
// compliance message instead of a generic block.
var ErrDomainDenied = errors.New("safehttp: domain is not permitted through this service (compliance policy)")

// deniedDomains is the fleet-wide hardcoded compliance denylist. These
// targets were named by our upstream proxy provider as
// copyright-infringement-related; continuing to route requests to them
// risks the proxy account being banned outright. Enforced centrally
// here (GuardHost — used by every safehttp client's dialer AND by
// CheckURL) so a single go-common version bump propagates the block to
// every consumer. Non-Go proxies (python-proxy, node-proxy,
// node-js-proxy, c-proxy) cannot import this package and keep their
// own hand-maintained copy of this list — update those alongside this
// one when the list changes.
var deniedDomains = map[string]struct{}{
	"sci-hub.st":       {},
	"sci-hub.ru":       {},
	"thepiratebay.org": {},
	"thepiratebay.se":  {},
	"rapidgator.net":   {},
	"avito.st":         {},
	"avito.ru":         {},
	"avitop.com":       {},
	"rutracker.org":    {},
	"ddos-guard.net":   {},
	"vglista.no":       {},
}

// IsDomainDenied reports whether host, or any parent domain of host, is
// on the compliance denylist. Matching is exact-or-subdomain: both
// "rutracker.org" and "tracker.rutracker.org" match the "rutracker.org"
// entry. host may be a bare hostname, "Host.With.Trailing.Dot.", or
// "host:port" — scheme prefixes are not stripped, so pass u.Hostname()
// rather than a raw URL string.
func IsDomainDenied(host string) bool {
	host = normHost(hostWithoutPort(host))
	if host == "" {
		return false
	}
	if _, ok := deniedDomains[host]; ok {
		return true
	}
	for d := range deniedDomains {
		if strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// hostWithoutPort strips a trailing ":port" if present. Unlike
// net.SplitHostPort it tolerates inputs with no port (returning them
// unchanged) instead of erroring, since callers pass both shapes.
func hostWithoutPort(host string) string {
	if i := strings.LastIndex(host, ":"); i != -1 && !strings.Contains(host, "]") {
		// Guard against bare IPv6 literals (multiple colons, no brackets)
		// being mis-split; only strip when there is exactly one colon.
		if strings.Count(host, ":") == 1 {
			return host[:i]
		}
	}
	return host
}

// DeniedDomains returns a sorted copy of the compliance denylist, for
// services that need to log, display, or cross-check the same list
// without going through IsDomainDenied (e.g. an admin UI, or a non-Go
// sibling's parity test fixture).
func DeniedDomains() []string {
	out := make([]string, 0, len(deniedDomains))
	for d := range deniedDomains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

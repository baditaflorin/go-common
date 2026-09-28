package graph

import (
	"strings"
)

// targetFromHost resolves a hostname to a fleet service slug.
// Fleet domains: <slug>.0exec.com, <slug>.0crawl.com. Returns
// "external:<host>" for anything else, so the collector can still
// see external dependencies without inflating slug cardinality.
//
// Internal LAN traffic (10.10.10.x) is best-resolved by the collector
// using services.json port lookups; here we just tag it generically.
func targetFromHost(host string) string {
	return targetFromHostWithAliases(host, nil)
}

func targetFromHostWithAliases(host string, aliases map[string]string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "external:unknown"
	}
	// Strip port if present.
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	if target, ok := aliases[host]; ok {
		return target
	}
	for _, suffix := range []string{".0exec.com", ".0crawl.com"} {
		if strings.HasSuffix(host, suffix) {
			slug := strings.TrimSuffix(host, suffix)
			if slug != "" {
				return slug
			}
		}
	}
	// LAN dockerhost; collector will resolve by port if possible.
	if strings.HasPrefix(host, "10.10.10.") || host == "localhost" || host == "127.0.0.1" {
		return "internal:" + host
	}
	return "external:" + host
}

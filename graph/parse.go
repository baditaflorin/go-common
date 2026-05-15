package graph

import (
	"strings"
)

// callerFromUA extracts the service slug from a fleet User-Agent.
// ua.Build emits "<serviceID>/<version> (+https://github.com/baditaflorin/<serviceID>)",
// e.g. "go_apikey_scanner/1.2.3 (+https://...)". The slug is the
// substring before the first "/".
//
// Returns "" if the UA doesn't match the fleet shape — we treat that
// as an external caller (browser, curl, k6, monitoring, etc.).
func callerFromUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	slash := strings.IndexByte(ua, '/')
	if slash <= 0 {
		return ""
	}
	id := ua[:slash]
	// Sanity: fleet service IDs are go_<snake> or kebab-case. Reject
	// well-known external UAs (Mozilla, curl, Go-http-client, ...).
	switch strings.ToLower(id) {
	case "mozilla", "curl", "wget", "go-http-client", "python-requests",
		"node-fetch", "okhttp", "java", "googlebot", "bingbot":
		return ""
	}
	// Require at least one of: go_ prefix OR kebab-case (contains '-').
	if !strings.HasPrefix(id, "go_") && !strings.Contains(id, "-") {
		return ""
	}
	return id
}

// targetFromHost resolves a hostname to a fleet service slug.
// Fleet domains: <slug>.0exec.com, <slug>.0crawl.com. Returns
// "external:<host>" for anything else, so the collector can still
// see external dependencies without inflating slug cardinality.
//
// Internal LAN traffic (10.10.10.x) is best-resolved by the collector
// using services.json port lookups; here we just tag it generically.
func targetFromHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "external:unknown"
	}
	// Strip port if present.
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
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

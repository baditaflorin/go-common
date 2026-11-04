// Package privatehost answers one question for the proxy-aware transports:
// can this hostname only mean something inside our own network?
//
// An external egress proxy cannot resolve a Docker compose container name
// (go_foo-app-1, go-fleet-graph), a bare single-label name, or a reserved
// internal suffix. Sending one costs a request and a connection to the proxy
// provider and always ends in target_connect_resolve_failed, so these names
// go direct (and fail locally and cheaply when they do not resolve) whatever
// NO_PROXY says and whether or not the name resolves here right now.
package privatehost

import "strings"

var internalSuffixes = []string{
	".internal",
	".local",
	".localhost",
	".lan",
	".home.arpa",
	".docker",
	".svc",
}

// Name reports whether host is a DNS name that cannot be public: it has no dot
// (a single label, which includes every compose service and container name) or
// ends in a reserved internal suffix. IP literals are not names; callers
// classify those by address. An empty host is not private.
func Name(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || strings.ContainsAny(host, ":[]") {
		return false
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, s := range internalSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

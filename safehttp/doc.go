// Package safehttp is the HTTP client for the PUBLIC internet: an *http.Client
// that refuses to connect to internal addresses (SSRF), plus the URL and IP
// checks it is built from.
//
// NewClient returns a client whose dialer rejects loopback, unspecified,
// multicast, link-local, private (RFC 1918), CGNAT and ULA targets. The check
// runs before DNS resolution and again at socket creation (Dialer.Control), so a
// hostname that rebinds to 169.254.169.254 after the first lookup is stopped
// too. A blocked target fails with ErrBlocked.
//
//	client := safehttp.NewClient(
//	    safehttp.WithTimeout(10*time.Second),
//	    safehttp.WithUserAgent(ua.Build(serviceID, version)),
//	)
//
// The pieces are usable on their own:
//
//   - NormalizeURL and ValidateURL parse and vet a user-supplied URL (http or
//     https, with a host); CheckURL adds the host guard in one call.
//   - IsBlocked, IsBlockedForHost and GuardHost classify an IP or a host name.
//   - WithEgressAllowlist and WithDenyAllEgress restrict the hosts a client may
//     reach at all.
//   - WithTraceCollector, WithBackoffCoordinator and WithDegradedSink wire the
//     client into the fleet's call tracing, backoff coordination and the
//     response envelope's degraded list.
//   - WithPersistentBreakerState keeps circuit-breaker state across restarts.
//   - WithoutFetchCache and WithoutFetchCacheContext skip the fleet fetch cache
//     for one client or one request.
//   - WithoutProxy and RequireProxy control whether the client uses the
//     configured egress proxy.
//
// # Not for calls inside the mesh
//
// The guard blocks private addresses on purpose, so a call to a sibling fleet
// service (http://go-fleet-*:<port>) through this client fails at once with
// ErrBlocked. Calls inside the mesh use a plain net/http.Client with a short
// timeout instead.
//
// # Environment
//
// SAFEHTTP_ALLOW_PRIVATE_IPS (a comma-separated list of literal IPs) and
// SAFEHTTP_ALLOW_PRIVATE_HOSTS exempt specific trusted targets from the private
// network check. See IsBlocked and IsBlockedForHost for why the IP-only
// allowlist is not safe for a service that fetches arbitrary caller-supplied
// host names.
package safehttp

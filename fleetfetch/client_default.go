package fleetfetch

// DefaultURL is the canonical fleet fetch-cache endpoint, addressed
// by Docker container DNS so producers in the same Docker network
// reach it without going through the public gateway (no public TLS
// handshake or proxy_egress detour through Webshare). The client still
// sends its configured service API key.
//
// Override at runtime via the FLEET_FETCH_CACHE_URL env var or per
// client via WithCacheURL. External callers (outside the fleet
// network) should set the env to the public URL:
//
//	FLEET_FETCH_CACHE_URL=https://infrastructure-fetch-cache.0exec.com
const DefaultURL = "http://go_infrastructure_fetch_cache:18205"

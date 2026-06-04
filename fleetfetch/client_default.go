package fleetfetch

// DefaultURL is the canonical fleet fetch-cache endpoint, addressed
// by Docker container DNS so producers in the same Docker network
// reach it without going through the public gateway (no TLS handshake,
// no keystore round-trip, no proxy_egress detour through Webshare).
//
// Override at runtime via the FLEET_FETCH_CACHE_URL env var or per
// client via WithCacheURL. External callers (outside the fleet
// network) should set the env to the public URL:
//
//	FLEET_FETCH_CACHE_URL=https://go-infrastructure-fetch-cache.0exec.com
const DefaultURL = "http://go_infrastructure_fetch_cache:18205"

// DefaultAPIKey is the pre-trusted local token (set via
// server.WithKeystoreAuth("default_token") on the cache container).
// NewClient defaults the API key to this when no override is given,
// so internal callers don't need any wiring to satisfy the cache's
// in-process keystore middleware. External callers should override
// via WithAPIKey or FLEET_FETCH_CACHE_API_KEY (the default_token is
// rate-limited at the public gateway).
const DefaultAPIKey = "default_token"

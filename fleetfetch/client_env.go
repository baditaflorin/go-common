package fleetfetch

// EnvCacheURL is the env var name read by NewClient when no
// WithCacheURL is set.
const EnvCacheURL = "FLEET_FETCH_CACHE_URL"

// EnvAPIKey is the env var name read by NewClient when no WithAPIKey
// is set.
const EnvAPIKey = "FLEET_FETCH_CACHE_API_KEY"

// EnvAPIKeyFile is a read-only mounted-secret path used when neither
// WithAPIKey nor EnvAPIKey supplies a credential.
const EnvAPIKeyFile = "FLEET_FETCH_CACHE_API_KEY_FILE"

// EnvSource is the process-wide default source read by NewClient when no
// WithSource option overrides it. Supported values are "live" and
// "commoncrawl". Common Crawl remains archive-only: a missing capture or
// cache error never falls back to live origin traffic.
const EnvSource = "FLEET_FETCH_SOURCE"

package fleetfetch

import "github.com/baditaflorin/go-common/credfile"

// EnvCacheURL is the env var name read by NewClient when no
// WithCacheURL is set.
const EnvCacheURL = "FLEET_FETCH_CACHE_URL"

// EnvAPIKey is the env var name read by NewClient when no WithAPIKey
// is set.
const EnvAPIKey = "FLEET_FETCH_CACHE_API_KEY"

// EnvAPIKeyFile is a read-only mounted-secret path used when neither
// WithAPIKey nor EnvAPIKey supplies a credential. Since 0.112.0 the file is
// followed (package credfile): a renewed key written over it is sent from the
// next request on, within a minute, or at once after the cache answers 401.
// FLEET_CREDFILE_RELOAD=off restores reading it once.
const EnvAPIKeyFile = "FLEET_FETCH_CACHE_API_KEY_FILE"

// APIKeyCredential is the credential name of the file-backed key in the
// fleet_credential_* metrics.
const APIKeyCredential = "fleet_fetch_cache_api_key"

// EnvSource is the process-wide default source read by NewClient when no
// request-scoped source or explicit WithSource option overrides it. Supported
// values are "live" and "commoncrawl". Common Crawl remains archive-only: a
// missing capture or cache error never falls back to live origin traffic.
const EnvSource = "FLEET_FETCH_SOURCE"

// apiKeyFileOptions: the old reader had no size limit and no permission
// check, so neither does this one beyond a 1 MiB bound. A variable so tests
// can shorten the check interval.
var apiKeyFileOptions = credfile.Options{MaxBytes: 1 << 20}

// attachAPIKeyFile follows the key file NewClient has just read. The start-up
// read stays NewClient's own (same errors as before); when the file cannot be
// followed (it changed in between, or is over 1 MiB), the client keeps the
// key it read, as it always did.
func (c *Client) attachAPIKeyFile(path string) {
	src, err := credfile.Open(APIKeyCredential, path, apiKeyFileOptions)
	if err != nil {
		return
	}
	c.apiKeySrc = src
	c.apiKey = src.Value()
}

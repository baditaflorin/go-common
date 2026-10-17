package jsbundle

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
)

// RecoverOptions tunes the pipeline.
type RecoverOptions struct {
	// Client fetches bundles and source maps. If nil, a safehttp client is
	// used: it refuses loopback, private, link-local and other internal
	// addresses (also after redirects), because the URLs come from pages and
	// error reports that the caller does not control. Pass your own client to
	// opt out, for example the httptest server's client in a test.
	Client         *http.Client
	UserAgent      string
	MaxScriptBytes int64
	MaxScripts     int
	MaxConcurrency int
}

// defaultFetchTimeout bounds one fetch made with the default client. Bundles can
// be a few MiB, so it is longer than safehttp's own 8 s.
const defaultFetchTimeout = 20 * time.Second

var (
	defaultClientMu sync.Mutex
	defaultClient   *http.Client
)

// guardedDefaultClient builds the default client on first use and shares it, so
// every Recover call reuses one connection pool. It is built lazily, on a
// request path or in a worker goroutine, so a deployment problem (for example
// FLEET_REQUIRE_PROXY=1 without a proxy variable) is returned as an error
// instead of panicking. A failure is not cached: the next call tries again, and
// never sees a nil client.
func guardedDefaultClient() (*http.Client, error) {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultClient != nil {
		return defaultClient, nil
	}
	c, err := safehttp.NewClientE(safehttp.WithTimeout(defaultFetchTimeout))
	if err != nil {
		return nil, err
	}
	defaultClient = c
	return c, nil
}

func (o *RecoverOptions) normalize() error {
	if o.Client == nil {
		c, err := guardedDefaultClient()
		if err != nil {
			return fmt.Errorf("jsbundle: default client: %w", err)
		}
		o.Client = c
	}
	if o.MaxScriptBytes <= 0 {
		o.MaxScriptBytes = DefaultMaxScriptBytes
	}
	if o.MaxScripts <= 0 {
		o.MaxScripts = DefaultMaxScripts
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = DefaultMaxConcurrency
	}
	if o.UserAgent == "" {
		o.UserAgent = "go-common-jsbundle/0.1"
	}
	return nil
}

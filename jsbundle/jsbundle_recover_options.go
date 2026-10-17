package jsbundle

import (
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
	defaultClientOnce sync.Once
	defaultClient     *http.Client
)

// guardedDefaultClient builds the default client once and shares it, so every
// Recover call reuses one connection pool.
func guardedDefaultClient() *http.Client {
	defaultClientOnce.Do(func() {
		defaultClient = safehttp.NewClient(safehttp.WithTimeout(defaultFetchTimeout))
	})
	return defaultClient
}

func (o *RecoverOptions) normalize() {
	if o.Client == nil {
		o.Client = guardedDefaultClient()
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
}

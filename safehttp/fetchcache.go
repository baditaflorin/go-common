package safehttp

import (
	"context"
	"net/http"
	"sync/atomic"
)

// FetchResult is a delegate's response to a routed GET. Body is the
// upstream response body verbatim; Header is the upstream response
// header set (may be nil). Status is the upstream HTTP status code.
type FetchResult struct {
	Status int
	Header http.Header
	Body   []byte
}

// FetchDelegate routes a plain GET through an alternate fetcher (e.g.
// the fleet fetch-cache). Implementations MUST be safe for concurrent
// use. A non-nil error signals "I couldn't serve this" — the caller
// (extrasTransport) falls through to the normal direct egress path, so
// a delegate outage never breaks the request.
//
// The concrete fleet-fetch-cache-backed adapter lives in go-common/
// server (which imports both safehttp and fleetfetch). safehttp itself
// must NOT import fleetfetch — that would create an import cycle, since
// fleetfetch imports safehttp for its SSRF-safe fallback. The interface
// here is the seam that keeps the dependency one-directional.
type FetchDelegate interface {
	FetchGet(ctx context.Context, url string, header http.Header) (*FetchResult, error)
}

// defaultFetchDelegate is the package-level delegate used by NewClient
// when the caller didn't pass WithFetchDelegate(...) and didn't opt out
// via WithoutFetchCache()/WithoutProxy(). Setters: SetDefaultFetchDelegate.
// Reads use atomic.Value so the hot path (NewClient) does not block on a
// mutex when no delegate is configured.
//
// The stored concrete type is always fetchDelegateHolder (never a bare
// interface value) — atomic.Value panics on Store of a nil interface and
// on Store of two different concrete types, so we wrap. A nil delegate is
// represented as a holder whose d field is nil rather than as a nil
// interface, which makes "clear the default" cheap and panic-free.
//
// Used by go-common/server.New to register a fleetfetch-backed delegate
// once per process when FLEET_FETCH_CACHE_URL is set: every safehttp
// client constructed afterwards transparently routes its outbound GETs
// through the fleet fetch cache (server-side singleflight + caching),
// with zero per-service code changes.
var defaultFetchDelegate atomic.Value // holds fetchDelegateHolder

// fetchDelegateHolder boxes a FetchDelegate so atomic.Value always sees
// a single, non-nil concrete type.
type fetchDelegateHolder struct{ d FetchDelegate }

// SetDefaultFetchDelegate installs a process-wide default fetch
// delegate. Calling it after NewClient has already constructed clients
// is safe but only affects subsequently-built clients — existing
// transports keep their original (nil or per-call) delegate.
//
// Pass nil to disable the default.
func SetDefaultFetchDelegate(d FetchDelegate) {
	defaultFetchDelegate.Store(fetchDelegateHolder{d: d})
}

// DefaultFetchDelegate returns the current process-wide default, or nil
// if none is set. Exposed mainly for tests and for server.New's wiring;
// production code does not need to read this directly.
func DefaultFetchDelegate() FetchDelegate {
	v := defaultFetchDelegate.Load()
	if v == nil {
		return nil
	}
	return v.(fetchDelegateHolder).d
}

// WithFetchDelegate attaches a FetchDelegate to this client. The
// delegate routes the client's eligible outbound GETs (no body, no
// Range header) through an alternate fetcher; on a delegate error the
// transport falls through to the normal direct path. A per-client
// delegate takes precedence over the process-wide DefaultFetchDelegate
// and applies even to WithoutProxy clients (the caller asked for it
// explicitly).
func WithFetchDelegate(d FetchDelegate) Option {
	return func(o *options) { o.fetchDelegate = d }
}

// WithoutFetchCache forces this client onto the direct egress path even
// when a process-wide DefaultFetchDelegate is installed. Use for clients
// that must observe real origin behavior (SSRF probers, smuggling
// detectors, latency/availability monitors) but that don't otherwise set
// WithoutProxy. An explicit WithFetchDelegate still wins over this flag.
func WithoutFetchCache() Option {
	return func(o *options) { o.noFetchCache = true }
}

// ctxKeyNoFetchCache is the private context key that disables fetch-cache
// delegate routing for a single request, regardless of how the client was
// constructed. Per-request opt-out, complementary to the per-client
// WithoutFetchCache() option.
type ctxKeyNoFetchCache struct{}

// WithoutFetchCacheContext returns a child context that disables fetch-cache
// delegate routing for any safehttp request made with it. Eligible GETs go
// direct to origin instead of through the process-wide DefaultFetchDelegate.
//
// This is a per-request override that works even on clients built before
// server.New installed the default delegate, and even on the package-level
// default client — without having to thread a WithoutFetchCache() option
// through every call site.
//
// The canonical user is the selftest suite: /selftest must validate the
// service's REAL outbound path (DNS + TLS + origin), not whatever the fleet
// cache happens to have warm. Routing selftest's live probes through a cold
// cache made them slow enough to trip `fleet-runner deploy`'s 8 s smoke
// /selftest timeout, false-failing otherwise-healthy deploys and rolling
// them back. Bypassing the cache for selftest keeps the gate fast and honest.
//
// An explicit per-client WithFetchDelegate still wins — a caller that wired
// a delegate on purpose is asking for it.
func WithoutFetchCacheContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKeyNoFetchCache{}, true)
}

// fetchCacheDisabledByContext reports whether ctx carries the
// WithoutFetchCacheContext opt-out flag.
func fetchCacheDisabledByContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyNoFetchCache{}).(bool)
	return v
}

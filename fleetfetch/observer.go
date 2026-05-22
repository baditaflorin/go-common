package fleetfetch

import (
	"net/url"
	"sync/atomic"
	"time"
)

// parseTargetURL wraps url.Parse so observer.go doesn't need an extra
// import group when client.go already pulls net/url. Split helper for
// the sole purpose of staying nil-tolerant on malformed inputs.
func parseTargetURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

// Observer receives one event per Client.fetch call. Implementations
// MUST NOT block — callbacks run inline on the producer hot path. The
// canonical implementation lives in go-common/promx and records
// fleet_fetch_total / latency / cache-age metrics.
//
// fleetfetch deliberately defines the contract here rather than
// importing a metrics library: fleetfetch keeps zero metric-stack
// deps.
type Observer interface {
	ObserveFleetFetch(Event)
}

// Event is the per-fetch payload handed to an Observer.
//
// Result is one of:
//
//	"hit"       — served from the cache, X-FetchCache-Hit=true
//	"miss"      — cache fetched upstream then returned to us
//	"fallback"  — cache unreachable (5xx / rejected / transport
//	              failure); we direct-fetched via safehttp
//	"timeout"   — cache reachable but too slow; we did NOT fall back
//	              (unless WithFallbackOnTimeout) — distinct from
//	              "fallback" so a slow cache doesn't pollute the
//	              direct-egress / proxy-bypass metric
//	"error"     — both cache and fallback failed, or caller context done
type Event struct {
	Host       string // hostname of the targetURL (not the cache)
	Result     string
	Status     int
	AgeSeconds int
	Duration   time.Duration
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer. Pass nil to
// disable. Wired by promx.AutoWire.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer or nil.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func emit(ev Event) {
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveFleetFetch(ev)
	}
}

func hostOf(rawURL string) string {
	u, err := parseTargetURL(rawURL)
	if err != nil || u == nil {
		return "_unknown"
	}
	h := u.Hostname()
	if h == "" {
		return "_unknown"
	}
	return h
}

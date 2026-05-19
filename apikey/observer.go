package apikey

import "time"

// CacheObserver receives one event per Cache.Verify call describing how
// the result was produced (fresh hit, stale hit during outage, upstream
// call). Implementations MUST NOT block — callbacks run on the auth hot
// path. The canonical implementation lives in go-common/promx.
//
// apikey deliberately defines the contract here rather than importing a
// metrics library: apikey keeps zero metric-stack deps, services pull in
// promx only when they want fleet metrics.
type CacheObserver interface {
	ObserveCache(CacheEvent)
}

// CacheEvent is the per-call payload handed to a CacheObserver.
type CacheEvent struct {
	Result CacheResult
	// Age is the cached entry's age at the moment it was served. Zero
	// when Result is InnerOK / InnerInvalid / InnerUnavailable (no
	// pre-existing entry was used). Useful to histogram "how stale was
	// the answer we gave?" during a keystore outage.
	Age time.Duration
	// Duration is the upstream call latency. Zero when the cache served
	// the answer without calling upstream (Fresh / Stale paths).
	Duration time.Duration
}

// CacheResult buckets the outcome.
type CacheResult string

const (
	CacheResultFresh            CacheResult = "fresh"             // cached < FreshTTL, no upstream call
	CacheResultStale            CacheResult = "stale"             // upstream unavailable, served stale entry < StaleTTL
	CacheResultInnerOK          CacheResult = "inner_ok"          // upstream returned a valid result (then cached)
	CacheResultInnerInvalid     CacheResult = "inner_invalid"     // upstream returned ErrInvalidKey (cache cleared)
	CacheResultInnerUnavailable CacheResult = "inner_unavailable" // upstream errored and no cache to fall back on
)

// AdminObserver receives one event per Client.Issue / Revoke / List /
// Purge call. Implementations MUST NOT block — callbacks run inline.
// The canonical implementation lives in go-common/promx and records
// apikey_admin_total{service, op, result} + duration histograms.
//
// Op is one of "issue", "revoke", "list", "purge". Result is one of
// "ok", "unauthorized", "unavailable", "client_error",
// "transport_error". Duration is the wall-clock time of the call.
type AdminObserver interface {
	ObserveAdmin(AdminEvent)
}

// AdminEvent is the per-admin-call payload handed to an AdminObserver.
type AdminEvent struct {
	Op       string
	Result   string
	Duration time.Duration
}

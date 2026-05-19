package depcheck

import "time"

// Observer receives one event per probed dependency per Snapshot call.
// Implementations MUST NOT block — callbacks run inline on /health.
// The canonical implementation lives in go-common/promx.
//
// depcheck deliberately defines the contract here rather than importing
// a metrics library: depcheck keeps zero metric-stack deps.
type Observer interface {
	ObserveDep(Event)
}

// Event is the per-probe payload handed to an Observer.
type Event struct {
	Dep     string
	OK      bool
	Latency time.Duration
	Err     string
}

// WithObserver returns a Registry option-equivalent. Apply it after
// New() via SetObserver — depcheck.New() doesn't take variadic options
// today, and we'd rather not break callers.
func (r *Registry) SetObserver(o Observer) *Registry {
	r.mu.Lock()
	r.observer = o
	r.mu.Unlock()
	return r
}

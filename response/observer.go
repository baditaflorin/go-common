package response

import "sync/atomic"

// Observer receives one event per Envelope() call. Implementations
// MUST NOT block — callbacks run inline on the response hot path. The
// canonical implementation lives in go-common/promx and records
// Prometheus counters for envelope emission and conflict warnings.
//
// response deliberately defines the contract here rather than
// importing a metrics library: response keeps zero metric-stack deps.
type Observer interface {
	ObserveEnvelope(Event)
}

// Event is the per-Envelope payload handed to an Observer.
//
// Warning is non-empty when the payload carried a reserved meta key
// (_service, _schema_version, _emitted_at) that conflicted with the
// envelope's own — the existing stderr log line is kept, this just
// makes it countable. SchemaVersion is the integer the caller passed
// (0 when omitted).
type Event struct {
	Service       string
	SchemaVersion int
	Warning       string
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide envelope observer. Pass
// nil to disable. Wired by promx.AutoWire when present so every
// Envelope call in the process produces metrics.
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

func emitEnvelope(ev Event) {
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveEnvelope(ev)
	}
}

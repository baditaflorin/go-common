package safehttp

import (
	"sync/atomic"
	"time"
)

// BackoffObserver receives one event per backoff-coordinator
// consultation (the WithBackoffCoordinator hook). Implementations MUST
// NOT block — callbacks run inline on the request hot path. The
// canonical implementation lives in go-common/promx and records
// safehttp_backoff_consult_total / duration.
//
// safehttp deliberately defines the contract here rather than
// importing a metrics library: safehttp keeps zero metric-stack deps.
type BackoffObserver interface {
	ObserveBackoff(BackoffEvent)
}

// BackoffEvent is the per-consultation payload handed to a
// BackoffObserver. Outcome is one of:
//
//	"consulted_no_wait"  — coordinator returned wait_ms=0
//	"consulted_waited"   — coordinator returned wait_ms>0 and we slept
//	"unreachable"        — coordinator network failure (fail-open)
type BackoffEvent struct {
	Host           string
	PriorStatus    int
	Outcome        string
	ConsultLatency time.Duration
	Waited         time.Duration
}

var defaultBackoffObserver atomic.Pointer[BackoffObserver]

// SetDefaultBackoffObserver installs a process-wide backoff observer.
// Pass nil to disable. Wired by promx.AutoWire.
func SetDefaultBackoffObserver(o BackoffObserver) {
	if o == nil {
		defaultBackoffObserver.Store(nil)
		return
	}
	defaultBackoffObserver.Store(&o)
}

// DefaultBackoffObserver returns the current process-wide observer or
// nil.
func DefaultBackoffObserver() BackoffObserver {
	p := defaultBackoffObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

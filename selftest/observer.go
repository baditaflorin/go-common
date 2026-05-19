package selftest

import "time"

// Observer receives one event per Suite.Render call describing the
// outcome of the run. Implementations MUST NOT block — callbacks run
// inline on the /selftest hot path. The canonical implementation lives
// in go-common/promx and records Prometheus counters/gauges/histograms.
//
// selftest deliberately defines the contract here rather than importing
// a metrics library: go-common/selftest keeps zero metric-stack deps,
// services pull in promx only when they want fleet metrics.
type Observer interface {
	ObserveSelftest(Event)
}

// Event is the per-render payload handed to an Observer. Checks is the
// same slice that lands in the JSON response, so consumers can build
// per-check labels (`fleet_selftest_check{service,check}`) without
// re-running the suite.
type Event struct {
	Service  string
	Version  string
	OK       bool
	Pass     int
	Fail     int
	Duration time.Duration
	Checks   []CheckResult
}

// WithObserver attaches an Observer to the Suite. The observer fires
// once per Render. Nil is a no-op so callers can wire conditionally.
func WithObserver(o Observer) Option {
	return func(s *Suite) {
		if o != nil {
			s.observer = o
		}
	}
}

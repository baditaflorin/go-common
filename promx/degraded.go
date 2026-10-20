package promx

import (
	"github.com/baditaflorin/go-common/degraded"
	"github.com/prometheus/client_golang/prometheus"
)

// DegradedCollectors records degraded.Sink.Append events for the
// fleet. Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	fleet_degraded_total{service, primitive, suffix}
//
// "primitive" is the upstream name (html-proxy, keystore, …);
// "suffix" is the per-token bucket (down, degraded, timeout,
// rate_limited). The token is whatever string the caller appended, and the
// degraded package invites a third-party host in it, so both labels are
// BoundedLabels: a process names at most 64 primitives and 16 suffixes
// (degradedPrimitiveLimit, degradedSuffixLimit) and counts the rest under
// "_other". A caller that reuses the same few primitive names, as the
// naming convention says, never sees the difference; one that appends a
// name per request used to mint a series per name.
type DegradedCollectors struct {
	service string

	primitives *BoundedLabel
	suffixes   *BoundedLabel

	appends *prometheus.CounterVec
}

// The most distinct "primitive" and "suffix" values fleet_degraded_total
// names per process. Services name their upstreams, a handful to a few dozen;
// the suffixes are the four of the naming convention and a few more.
const (
	degradedPrimitiveLimit = 64
	degradedSuffixLimit    = 16
)

// NewDegradedCollectors registers the degraded-sink collector on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewDegradedCollectors(reg prometheus.Registerer) *DegradedCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &DegradedCollectors{
		service:    ServiceID(),
		primitives: NewBoundedLabel(degradedPrimitiveLimit),
		suffixes:   NewBoundedLabel(degradedSuffixLimit),
		appends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_degraded_total",
			Help: "Total degraded-sink appends, labelled by primitive and suffix (down/degraded/timeout/...).",
		}, []string{"service", "primitive", "suffix"}),
	}
	reg.MustRegister(c.appends)
	return c
}

// ObserveDegraded satisfies degraded.Observer.
func (c *DegradedCollectors) ObserveDegraded(ev degraded.Event) {
	suffix := ev.Suffix
	if suffix == "" {
		suffix = "_none"
	}
	c.appends.WithLabelValues(c.service, c.primitives.Value(ev.Primitive), c.suffixes.Value(suffix)).Inc()
}

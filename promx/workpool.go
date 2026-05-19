package promx

import (
	"github.com/baditaflorin/go-common/workpool"
	"github.com/prometheus/client_golang/prometheus"
)

// WorkpoolCollectors records workpool.Pool lifecycle events for the
// fleet. Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	pool_inflight{service, pool}            // gauge: current in-flight tasks
//	pool_queue_depth{service, pool}         // gauge: callers blocked in Submit
//	pool_phase_total{service, pool, phase}  // counter: queued / started / finished / canceled / shed
//
// Saturation pattern: alert on
//
//	`pool_inflight / on(service, pool) group_left pool_size >= 1`
//
// once we publish pool_size as a separate gauge — for now,
// `pool_queue_depth > 0` is the canonical "you're starved" signal.
type WorkpoolCollectors struct {
	service string

	inflight   *prometheus.GaugeVec
	queueDepth *prometheus.GaugeVec
	phaseTotal *prometheus.CounterVec
}

// NewWorkpoolCollectors registers the workpool collectors on reg. reg
// may be nil — the shared promx.Registry() is used in that case.
func NewWorkpoolCollectors(reg prometheus.Registerer) *WorkpoolCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &WorkpoolCollectors{
		service: ServiceID(),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_inflight",
			Help: "Current number of in-flight tasks in a workpool.",
		}, []string{"service", "pool"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pool_queue_depth",
			Help: "Current number of callers blocked in Submit waiting for a slot.",
		}, []string{"service", "pool"}),
		phaseTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pool_phase_total",
			Help: "Total workpool lifecycle events, labelled by phase (queued/started/finished/canceled/shed).",
		}, []string{"service", "pool", "phase"}),
	}
	reg.MustRegister(c.inflight, c.queueDepth, c.phaseTotal)
	return c
}

// ObserveWorkpool satisfies workpool.Observer.
func (c *WorkpoolCollectors) ObserveWorkpool(ev workpool.Event) {
	c.inflight.WithLabelValues(c.service, ev.Pool).Set(float64(ev.InFlight))
	c.queueDepth.WithLabelValues(c.service, ev.Pool).Set(float64(ev.QueueDepth))
	c.phaseTotal.WithLabelValues(c.service, ev.Pool, string(ev.Phase)).Inc()
}

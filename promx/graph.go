package promx

import (
	"github.com/baditaflorin/go-common/graph"
	"github.com/prometheus/client_golang/prometheus"
)

// GraphCollectors exports the Fleet Graph sender's own counters, read from
// graph.Stats() at scrape time.
//
// Metrics exposed (all counters, labelled service):
//
//	fleet_graph_events_recorded_total
//	fleet_graph_events_dropped_total    ring overflow, oldest event evicted
//	fleet_graph_batches_sent_total
//	fleet_graph_batches_failed_total    every non-delivered batch (includes rejected)
//	fleet_graph_batches_rejected_total  batches the collector refused for their
//	                                    content (403, 400, 413, 415, 422) and
//	                                    that were dropped, so events were lost
//	fleet_graph_events_dropped_invalid_identity_total
//	                                    events dropped because GRAPH_SERVICE_ID is
//	                                    set but invalid (identity fails closed, so
//	                                    the service emits nothing until fixed)
//	fleet_graph_batches_sent_spiffe_total
//	                                    the part of batches_sent delivered over the
//	                                    SPIFFE mTLS path, without the writer key
//	fleet_graph_batches_spiffe_fallback_total
//	                                    batches the SPIFFE path failed to deliver
//	                                    that were sent with the writer key instead
//
// A service whose telemetry silently stops arriving shows up here as
// batches_failed/batches_rejected rising while batches_sent stays flat, or, for
// a malformed GRAPH_SERVICE_ID, as events_dropped_invalid_identity rising while
// events_recorded stays at zero.
type GraphCollectors struct {
	service string
}

// NewGraphCollectors registers the graph counters on reg. Registration is
// best-effort: a name already registered (a service with its own fleet_graph_*
// metric) is skipped rather than failing start-up.
func NewGraphCollectors(reg prometheus.Registerer, serviceID string) *GraphCollectors {
	if serviceID == "" {
		serviceID = "_unknown"
	}
	c := &GraphCollectors{service: serviceID}
	if reg == nil {
		return c
	}
	for _, m := range []struct {
		name, help string
		read       func(graph.Counters) int64
	}{
		{"fleet_graph_events_recorded_total", "Fleet Graph events recorded into the send ring.", func(s graph.Counters) int64 { return s.EventsRecorded }},
		{"fleet_graph_events_dropped_total", "Fleet Graph events evicted from the full send ring before being sent.", func(s graph.Counters) int64 { return s.EventsDropped }},
		{"fleet_graph_batches_sent_total", "Fleet Graph batches the collector accepted.", func(s graph.Counters) int64 { return s.BatchesSent }},
		{"fleet_graph_batches_failed_total", "Fleet Graph batches that were not delivered (includes rejected batches).", func(s graph.Counters) int64 { return s.BatchesFailed }},
		{"fleet_graph_batches_rejected_total", "Fleet Graph batches the collector refused for their content and that were dropped, so their events are lost.", func(s graph.Counters) int64 { return s.BatchesRejected }},
		{"fleet_graph_events_dropped_invalid_identity_total", "Fleet Graph events dropped because GRAPH_SERVICE_ID is set but is not a valid service ID, so Graph emission is off for the process.", func(s graph.Counters) int64 { return s.EventsDroppedInvalidIdentity }},
		{"fleet_graph_batches_sent_spiffe_total", "Fleet Graph batches the collector accepted over the SPIFFE mTLS path (a subset of batches_sent).", func(s graph.Counters) int64 { return s.BatchesSentSPIFFE }},
		{"fleet_graph_batches_spiffe_fallback_total", "Fleet Graph batches the SPIFFE mTLS path failed to deliver that were sent with the writer key instead.", func(s graph.Counters) int64 { return s.BatchesSPIFFEFallback }},
	} {
		read := m.read
		_ = reg.Register(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name:        m.name,
			Help:        m.help,
			ConstLabels: prometheus.Labels{"service": serviceID},
		}, func() float64 { return float64(read(graph.Stats())) }))
	}
	return c
}

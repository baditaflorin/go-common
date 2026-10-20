package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/graph"
	"github.com/prometheus/client_golang/prometheus"
)

func TestGraphCollectorsExportSenderCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewGraphCollectors(reg, "go-graph-test")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var exportedRejected, exportedInvalid float64
	for _, f := range families {
		m := f.Metric[0]
		if len(m.Label) != 1 || m.Label[0].GetName() != "service" || m.Label[0].GetValue() != "go-graph-test" {
			t.Errorf("%s labels = %v, want service=go-graph-test", f.GetName(), m.Label)
		}
		got[f.GetName()] = f.GetType().String()
		if f.GetName() == "fleet_graph_batches_rejected_total" {
			exportedRejected = m.GetCounter().GetValue()
		}
		if f.GetName() == "fleet_graph_events_dropped_invalid_identity_total" {
			exportedInvalid = m.GetCounter().GetValue()
		}
	}
	for _, name := range []string{
		"fleet_graph_events_recorded_total", "fleet_graph_events_dropped_total",
		"fleet_graph_batches_sent_total", "fleet_graph_batches_failed_total",
		"fleet_graph_batches_rejected_total", "fleet_graph_events_dropped_invalid_identity_total",
		"fleet_graph_batches_sent_spiffe_total", "fleet_graph_batches_spiffe_fallback_total",
	} {
		if got[name] != "COUNTER" {
			t.Errorf("%s missing or not a counter (families: %v)", name, got)
		}
	}
	// Stats() is the single source: the exported value must follow it.
	if want := float64(graph.Stats().BatchesRejected); exportedRejected != want {
		t.Errorf("batches_rejected exported %v, want graph.Stats value %v", exportedRejected, want)
	}
	if want := float64(graph.Stats().EventsDroppedInvalidIdentity); exportedInvalid != want {
		t.Errorf("events_dropped_invalid_identity exported %v, want graph.Stats value %v", exportedInvalid, want)
	}
}

func TestGraphCollectorsSkipAnAlreadyRegisteredName(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "fleet_graph_batches_sent_total", Help: "service-owned",
		ConstLabels: prometheus.Labels{"service": "go-graph-test"},
	}, func() float64 { return 0 }))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewGraphCollectors panicked on a name collision: %v", r)
		}
	}()
	NewGraphCollectors(reg, "go-graph-test")
}

func TestGraphCollectorsSkipOnlyTheCollidingInvalidIdentityName(t *testing.T) {
	// Best-effort registration is per name: a service-owned
	// fleet_graph_events_dropped_invalid_identity_total must not stop the other
	// collectors from registering, and must not panic.
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "fleet_graph_events_dropped_invalid_identity_total", Help: "service-owned",
		ConstLabels: prometheus.Labels{"service": "go-graph-test"},
	}, func() float64 { return 0 }))
	NewGraphCollectors(reg, "go-graph-test")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
	}
	for _, want := range []string{"fleet_graph_events_recorded_total", "fleet_graph_batches_rejected_total"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("%s not registered after a colliding name (families: %v)", want, names)
		}
	}
}

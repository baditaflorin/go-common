package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCanceledProbeIsObservedWithoutCountingUpstreamTrip(t *testing.T) {
	collectors := NewCircuitCollectors(prometheus.NewRegistry())
	upstream := "dependency"
	collectors.ObserveCircuit(circuitbreaker.Event{Upstream: upstream, From: circuitbreaker.StateClosed, To: circuitbreaker.StateOpen, Reason: "threshold_reached"})
	collectors.ObserveCircuit(circuitbreaker.Event{Upstream: upstream, From: circuitbreaker.StateHalfOpen, To: circuitbreaker.StateOpen, Reason: "probe_canceled"})
	if got := testutil.ToFloat64(collectors.trips.WithLabelValues(collectors.service, upstream)); got != 1 {
		t.Fatalf("neutral cancellation inflated upstream trips to %v", got)
	}
	if got := testutil.ToFloat64(collectors.transitions.WithLabelValues(collectors.service, upstream, "half_open", "open", "probe_canceled")); got != 1 {
		t.Fatalf("neutral transition count=%v", got)
	}
	if got := testutil.ToFloat64(collectors.state.WithLabelValues(collectors.service, upstream)); got != float64(circuitbreaker.StateOpen) {
		t.Fatalf("state gauge=%v", got)
	}
	collectors.ObserveCircuit(circuitbreaker.Event{Upstream: upstream, From: circuitbreaker.StateHalfOpen, To: circuitbreaker.StateOpen, Reason: "probe_failed"})
	if got := testutil.ToFloat64(collectors.trips.WithLabelValues(collectors.service, upstream)); got != 2 {
		t.Fatalf("real probe failure did not count: trips=%v", got)
	}
}

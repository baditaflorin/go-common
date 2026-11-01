package promx

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestFleetFetchIdentityMetric(t *testing.T) {
	c := NewFleetFetchCollectors(prometheus.NewRegistry())
	// Not enabled (the default): no series.
	c.ObserveFleetFetch(fleetfetch.Event{Host: "example.com", Result: "hit", Duration: time.Millisecond})
	if n := testutil.CollectAndCount(c.identity); n != 0 {
		t.Fatalf("%d identity series without the SPIFFE path enabled", n)
	}
	c.ObserveFleetFetch(fleetfetch.Event{Host: "example.com", Result: "hit", Identity: fleetfetch.IdentitySPIFFE})
	c.ObserveFleetFetch(fleetfetch.Event{Host: "example.com", Result: "miss", Identity: fleetfetch.IdentitySPIFFE})
	c.ObserveFleetFetch(fleetfetch.Event{Host: "example.com", Result: "hit", Identity: fleetfetch.IdentityUnavailable})
	c.ObserveFleetFetch(fleetfetch.Event{Host: "example.com", Result: "hit", Identity: "free text ak_123"})
	for id, want := range map[string]float64{"spiffe": 2, "unavailable": 1, "other": 1} {
		if got := testutil.ToFloat64(c.identity.WithLabelValues(ServiceID(), id)); got != want {
			t.Errorf("identity=%q: %v, want %v", id, got, want)
		}
	}
}

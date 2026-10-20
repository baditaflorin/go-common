package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fleet_fetch_truncated_total counts only fetches whose response was cut, and has
// no host label: a service's rate is one series, whatever sites it fetches.
func TestFleetFetchTruncatedCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewFleetFetchCollectors(reg)
	for i, trunc := range []bool{false, true, true, false} {
		c.ObserveFleetFetch(fleetfetch.Event{Host: "site" + string(rune('a'+i)) + ".example", Result: "miss", Status: 200, Truncated: trunc})
	}
	if got := testutil.ToFloat64(c.truncated.WithLabelValues(c.service)); got != 2 {
		t.Fatalf("fleet_fetch_truncated_total = %v, want 2", got)
	}
	if n := testutil.CollectAndCount(c.truncated); n != 1 {
		t.Fatalf("series = %d, want 1 (service only)", n)
	}
}

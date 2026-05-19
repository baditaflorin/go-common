package promx

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/ratecoord"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRateCoordCollectors_Outcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewRateCoordCollectors(reg)

	// Remote success.
	c.ObserveRate(ratecoord.Event{Host: "a.example", Weight: 1, Waited: 5 * time.Millisecond, Allowed: true, FellBack: false})
	// Fallback success (coordinator unreachable, local bucket let us through).
	c.ObserveRate(ratecoord.Event{Host: "b.example", Weight: 1, Waited: 200 * time.Millisecond, Allowed: true, FellBack: true})
	// Fallback denied (local bucket said no within maxWait).
	c.ObserveRate(ratecoord.Event{Host: "b.example", Weight: 1, Waited: 5 * time.Second, Allowed: false, FellBack: true})

	if v := testutil.ToFloat64(c.decisions.WithLabelValues(c.service, "a.example", "allowed")); v != 1 {
		t.Fatalf("decisions(a,allowed) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.decisions.WithLabelValues(c.service, "b.example", "fallback_allowed")); v != 1 {
		t.Fatalf("decisions(b,fallback_allowed) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.decisions.WithLabelValues(c.service, "b.example", "fallback_denied")); v != 1 {
		t.Fatalf("decisions(b,fallback_denied) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.fallback.WithLabelValues(c.service)); v != 2 {
		t.Fatalf("fallback total = %v, want 2", v)
	}
}

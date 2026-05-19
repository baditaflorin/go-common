package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/degraded"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDegradedCollectors_ObserveDegraded(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewDegradedCollectors(reg)

	c.ObserveDegraded(degraded.Event{Token: "html-proxy-down", Primitive: "html-proxy", Suffix: "down"})
	c.ObserveDegraded(degraded.Event{Token: "html-proxy-down", Primitive: "html-proxy", Suffix: "down"})
	c.ObserveDegraded(degraded.Event{Token: "keystore", Primitive: "keystore", Suffix: ""})

	if v := testutil.ToFloat64(c.appends.WithLabelValues(c.service, "html-proxy", "down")); v != 2 {
		t.Fatalf("appends(html-proxy,down) = %v, want 2", v)
	}
	if v := testutil.ToFloat64(c.appends.WithLabelValues(c.service, "keystore", "_none")); v != 1 {
		t.Fatalf("appends(keystore,_none) = %v, want 1", v)
	}
}

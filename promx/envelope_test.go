package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/response"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEnvelopeCollectors_ObserveEnvelope(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewEnvelopeCollectors(reg)

	c.ObserveEnvelope(response.Event{Service: "svc", SchemaVersion: 3})
	c.ObserveEnvelope(response.Event{Service: "svc", SchemaVersion: 3, Warning: "conflict__service"})

	if v := testutil.ToFloat64(c.emitted.WithLabelValues(c.service, "3")); v != 2 {
		t.Fatalf("emitted = %v, want 2", v)
	}
	if v := testutil.ToFloat64(c.warnings.WithLabelValues(c.service, "conflict__service")); v != 1 {
		t.Fatalf("warnings = %v, want 1", v)
	}
}

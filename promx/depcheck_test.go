package promx

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/depcheck"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDepCollectors_ObserveDep(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewDepCollectors(reg)

	c.ObserveDep(depcheck.Event{Dep: "html-proxy", OK: true, Latency: 12 * time.Millisecond})
	c.ObserveDep(depcheck.Event{Dep: "keystore", OK: false, Latency: 3 * time.Second, Err: "timeout"})

	if v := testutil.ToFloat64(c.ok.WithLabelValues(c.service, "html-proxy")); v != 1 {
		t.Fatalf("ok(html-proxy) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.ok.WithLabelValues(c.service, "keystore")); v != 0 {
		t.Fatalf("ok(keystore) = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.runs.WithLabelValues(c.service, "keystore", "fail")); v != 1 {
		t.Fatalf("runs(keystore,fail) = %v, want 1", v)
	}
}

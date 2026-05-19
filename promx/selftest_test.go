package promx

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/selftest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSelftestCollectors_ObserveSelftest(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewSelftestCollectors(reg)

	c.ObserveSelftest(selftest.Event{
		Service:  "svc-a",
		Version:  "1.0.0",
		OK:       false,
		Pass:     2,
		Fail:     1,
		Duration: 250 * time.Millisecond,
		Checks: []selftest.CheckResult{
			{Name: "dns", Pass: true},
			{Name: "upstream", Pass: false, Err: "boom"},
			{Name: "corpus", Pass: true},
		},
	})

	if v := testutil.ToFloat64(c.runs.WithLabelValues(c.service, "fail")); v != 1 {
		t.Fatalf("runs(fail) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.checkPass.WithLabelValues(c.service, "upstream")); v != 0 {
		t.Fatalf("checkPass(upstream) = %v, want 0", v)
	}
	if v := testutil.ToFloat64(c.checkPass.WithLabelValues(c.service, "dns")); v != 1 {
		t.Fatalf("checkPass(dns) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.checkRuns.WithLabelValues(c.service, "upstream", "fail")); v != 1 {
		t.Fatalf("checkRuns(upstream,fail) = %v, want 1", v)
	}
}

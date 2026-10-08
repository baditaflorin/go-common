package promx

import (
	"errors"
	"testing"

	"github.com/baditaflorin/go-common/cgroup"
	"github.com/prometheus/client_golang/prometheus"
)

func TestCgroupCollectorsExportSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	snapshot := cgroup.Snapshot{
		MemoryUsageBytes: u64(100), MemoryLimitBytes: u64(250),
		CPUUsageSeconds: f64(2.5), CPUQuotaCores: f64(1.5),
		CPUThrottledPeriods: u64(3), CPUThrottledSeconds: f64(0.2),
		CPUPressureSome: &cgroup.Pressure{Avg10Ratio: f64(0.1)},
	}
	reg.MustRegister(newCgroupCollectors("go-proxy", func() (cgroup.Snapshot, error) { return snapshot, nil }))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]float64)
	for _, family := range families {
		metric := family.Metric[0]
		if metric.Gauge != nil {
			got[family.GetName()] = metric.GetGauge().GetValue()
		} else {
			got[family.GetName()] = metric.GetCounter().GetValue()
		}
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "service" || metric.Label[0].GetValue() != "go-proxy" {
			t.Errorf("%s labels=%v, want service=go-proxy", family.GetName(), metric.Label)
		}
	}
	for name, want := range map[string]float64{
		"go_container_resource_read_error": 0, "go_container_memory_usage_bytes": 100,
		"go_container_memory_limit_bytes": 250, "go_container_memory_available_bytes": 150,
		"go_container_cpu_usage_seconds_total": 2.5, "go_container_cpu_quota_cores": 1.5,
		"go_container_cpu_throttled_periods_total": 3, "go_container_cpu_throttled_seconds_total": 0.2,
		"go_container_cpu_pressure_some_avg10_ratio": 0.1,
	} {
		if got[name] != want {
			t.Errorf("%s=%v, want %v", name, got[name], want)
		}
	}
}

func TestCgroupCollectorReadErrorIsObservable(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newCgroupCollectors("go-proxy", func() (cgroup.Snapshot, error) {
		return cgroup.Snapshot{}, errors.New("unavailable")
	}))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "go_container_resource_read_error" ||
		families[0].Metric[0].GetGauge().GetValue() != 1 {
		t.Fatalf("unexpected metrics after read failure: %v", families)
	}
}

func u64(v uint64) *uint64   { return &v }
func f64(v float64) *float64 { return &v }

package promx

import (
	"github.com/baditaflorin/go-common/cgroup"
	"github.com/prometheus/client_golang/prometheus"
)

// CgroupCollectors exports the current process cgroup's limits, use,
// throttling, and Linux PSI pressure metrics. Host capacity remains the host
// metrics agent's responsibility.
type CgroupCollectors struct {
	service string
	read    func() (cgroup.Snapshot, error)
	desc    map[string]*prometheus.Desc
}

// NewCgroupCollectors registers cgroup metrics on reg. serviceID is a static
// service identifier used as the only label.
func NewCgroupCollectors(reg prometheus.Registerer, serviceID string) *CgroupCollectors {
	c := newCgroupCollectors(serviceID, cgroup.Read)
	if reg != nil {
		reg.MustRegister(c)
	}
	return c
}

func newCgroupCollectors(serviceID string, read func() (cgroup.Snapshot, error)) *CgroupCollectors {
	if serviceID == "" {
		serviceID = "_unknown"
	}
	help := map[string]string{
		"resource_read_error":                "Whether the current process cgroup snapshot could be read.",
		"memory_usage_bytes":                 "Current cgroup memory usage in bytes.",
		"memory_limit_bytes":                 "Cgroup memory limit in bytes; absent when unlimited or unavailable.",
		"memory_available_bytes":             "Remaining cgroup memory limit in bytes.",
		"cpu_usage_seconds_total":            "Cumulative CPU time consumed by the cgroup in seconds.",
		"cpu_quota_cores":                    "Cgroup CPU quota in cores; absent when unlimited or unavailable.",
		"cpu_throttled_periods_total":        "Cumulative cgroup CPU throttled periods.",
		"cpu_throttled_seconds_total":        "Cumulative cgroup CPU throttled time in seconds.",
		"cpu_pressure_some_avg10_ratio":      "Fraction of the last 10 seconds with cgroup tasks stalled on CPU.",
		"cpu_pressure_some_avg60_ratio":      "Fraction of the last 60 seconds with cgroup tasks stalled on CPU.",
		"cpu_pressure_some_avg300_ratio":     "Fraction of the last 300 seconds with cgroup tasks stalled on CPU.",
		"cpu_pressure_some_seconds_total":    "Cumulative seconds with cgroup tasks stalled on CPU.",
		"memory_pressure_some_avg10_ratio":   "Fraction of the last 10 seconds with cgroup tasks stalled on memory.",
		"memory_pressure_some_avg60_ratio":   "Fraction of the last 60 seconds with cgroup tasks stalled on memory.",
		"memory_pressure_some_avg300_ratio":  "Fraction of the last 300 seconds with cgroup tasks stalled on memory.",
		"memory_pressure_some_seconds_total": "Cumulative seconds with cgroup tasks stalled on memory.",
		"memory_pressure_full_avg10_ratio":   "Fraction of the last 10 seconds with all non-idle cgroup tasks stalled on memory.",
		"memory_pressure_full_avg60_ratio":   "Fraction of the last 60 seconds with all non-idle cgroup tasks stalled on memory.",
		"memory_pressure_full_avg300_ratio":  "Fraction of the last 300 seconds with all non-idle cgroup tasks stalled on memory.",
		"memory_pressure_full_seconds_total": "Cumulative seconds with all non-idle cgroup tasks stalled on memory.",
	}
	desc := make(map[string]*prometheus.Desc, len(help))
	for name, text := range help {
		desc[name] = prometheus.NewDesc("go_container_"+name, text, []string{"service"}, nil)
	}
	return &CgroupCollectors{service: serviceID, read: read, desc: desc}
}

func (c *CgroupCollectors) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.desc {
		ch <- desc
	}
}

func (c *CgroupCollectors) Collect(ch chan<- prometheus.Metric) {
	s, err := c.read()
	if err != nil {
		c.emit(ch, "resource_read_error", prometheus.GaugeValue, 1)
		return
	}
	c.emit(ch, "resource_read_error", prometheus.GaugeValue, 0)
	if s.MemoryUsageBytes != nil {
		c.emit(ch, "memory_usage_bytes", prometheus.GaugeValue, float64(*s.MemoryUsageBytes))
	}
	if s.MemoryLimitBytes != nil {
		c.emit(ch, "memory_limit_bytes", prometheus.GaugeValue, float64(*s.MemoryLimitBytes))
		if s.MemoryUsageBytes != nil {
			available := uint64(0)
			if *s.MemoryLimitBytes > *s.MemoryUsageBytes {
				available = *s.MemoryLimitBytes - *s.MemoryUsageBytes
			}
			c.emit(ch, "memory_available_bytes", prometheus.GaugeValue, float64(available))
		}
	}
	if s.CPUUsageSeconds != nil {
		c.emit(ch, "cpu_usage_seconds_total", prometheus.CounterValue, *s.CPUUsageSeconds)
	}
	if s.CPUQuotaCores != nil {
		c.emit(ch, "cpu_quota_cores", prometheus.GaugeValue, *s.CPUQuotaCores)
	}
	if s.CPUThrottledPeriods != nil {
		c.emit(ch, "cpu_throttled_periods_total", prometheus.CounterValue, float64(*s.CPUThrottledPeriods))
	}
	if s.CPUThrottledSeconds != nil {
		c.emit(ch, "cpu_throttled_seconds_total", prometheus.CounterValue, *s.CPUThrottledSeconds)
	}
	c.emitPressure(ch, "cpu_pressure_some", s.CPUPressureSome)
	c.emitPressure(ch, "memory_pressure_some", s.MemoryPressureSome)
	c.emitPressure(ch, "memory_pressure_full", s.MemoryPressureFull)
}

func (c *CgroupCollectors) emit(ch chan<- prometheus.Metric, name string, kind prometheus.ValueType, value float64) {
	ch <- prometheus.MustNewConstMetric(c.desc[name], kind, value, c.service)
}

func (c *CgroupCollectors) emitPressure(ch chan<- prometheus.Metric, prefix string, p *cgroup.Pressure) {
	if p == nil {
		return
	}
	for _, item := range []struct {
		name  string
		value *float64
	}{
		{"avg10_ratio", p.Avg10Ratio},
		{"avg60_ratio", p.Avg60Ratio},
		{"avg300_ratio", p.Avg300Ratio},
		{"seconds_total", p.TotalSeconds},
	} {
		if item.value != nil {
			kind := prometheus.GaugeValue
			if item.name == "seconds_total" {
				kind = prometheus.CounterValue
			}
			c.emit(ch, prefix+"_"+item.name, kind, *item.value)
		}
	}
}

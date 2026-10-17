package chromeguard

import (
	"runtime"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/cgroup"
)

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }

const gib = 1 << 30

func TestSize(t *testing.T) {
	cases := []struct {
		name         string
		snap         cgroup.Snapshot
		opts         SizingOptions
		browsers     int
		tabs         int
		reasonHas    string
		reasonHasNot string
	}{
		{
			name: "8 CPU / 6 GiB is memory bound",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(8), MemoryLimitBytes: u64(6 * gib)},
			// 20 tabs by cpu; 14 tabs + 3 browsers = 3.5+0.6 GiB fits 4.2 GiB, 15 does not.
			browsers: 3, tabs: 14, reasonHas: "limited by memory",
		},
		{
			name:     "2 CPU / 2 GiB",
			snap:     cgroup.Snapshot{CPUQuotaCores: f64(2), MemoryLimitBytes: u64(2 * gib)},
			browsers: 1, tabs: 4, reasonHas: "limited by memory",
		},
		{
			name: "4 CPU / 1 GiB: memory below the minimum",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(4), MemoryLimitBytes: u64(1 * gib)},
			// Not even 4 tabs fit in 0.7 GiB; the floor still applies and the reason says why.
			browsers: 1, tabs: 4, reasonHas: "memory too small",
		},
		{
			name:     "plenty of memory is cpu bound",
			snap:     cgroup.Snapshot{CPUQuotaCores: f64(4), MemoryLimitBytes: u64(32 * gib)},
			browsers: 2, tabs: 10, reasonHas: "limited by cpu",
		},
		{
			name:     "nil limits use the host values",
			snap:     cgroup.Snapshot{},
			opts:     SizingOptions{HostCPUs: 4, HostMemBytes: 16 * gib},
			browsers: 2, tabs: 10, reasonHas: "host cpus (no quota)",
		},
		{
			name: "nil memory limit only",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(8)},
			opts: SizingOptions{HostMemBytes: 3 * gib},
			// 3 GiB * 0.7 = 2150 MiB: 7 tabs + 2 browsers = 1750+400 fits, 8 tabs = 2000+400 does not.
			browsers: 2, tabs: 7, reasonHas: "host memory (no limit)",
		},
		{
			name: "a quota above the host's cpus is clamped",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(64), MemoryLimitBytes: u64(64 * gib)},
			opts: SizingOptions{HostCPUs: 4},
			// 4 cpus * 2.5 = 10 tabs
			browsers: 2, tabs: 10, reasonHas: "quota is higher",
		},
		{
			name:     "huge machine is capped by MaxTabs and MaxBrowsers",
			snap:     cgroup.Snapshot{CPUQuotaCores: f64(64), MemoryLimitBytes: u64(256 * gib)},
			browsers: 6, tabs: 40, reasonHas: "limited by MaxTabs",
		},
		{
			name:     "tiny quota is raised to MinTabs",
			snap:     cgroup.Snapshot{CPUQuotaCores: f64(0.5), MemoryLimitBytes: u64(8 * gib)},
			browsers: 1, tabs: 4, reasonHas: "raised to MinTabs",
		},
		{
			name: "custom options",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(8), MemoryLimitBytes: u64(8 * gib)},
			opts: SizingOptions{TabsPerCPU: 1, UsableFraction: 0.5, PerTabBytes: 100 << 20, BaseBrowserBytes: 100 << 20,
				MinTabs: 2, MaxTabs: 30, TabsPerBrowser: 4, MaxBrowsers: 3},
			// 8 tabs by cpu, 2 browsers, 100M*2 + 8*100M = 1000M <= 4G
			browsers: 2, tabs: 8, reasonHas: "limited by cpu",
		},
		{
			name: "MaxBrowsers caps browsers while tabs keep their number",
			snap: cgroup.Snapshot{CPUQuotaCores: f64(16), MemoryLimitBytes: u64(64 * gib)},
			opts: SizingOptions{TabsPerBrowser: 3, MaxBrowsers: 4},
			// 16*2.5 = 40 tabs; ceil(40/3) = 14 browsers capped to 4
			browsers: 4, tabs: 40, reasonHas: "limited by cpu",
		},
		{
			name:     "invalid options fall back to the defaults",
			snap:     cgroup.Snapshot{CPUQuotaCores: f64(8), MemoryLimitBytes: u64(6 * gib)},
			opts:     SizingOptions{TabsPerCPU: -1, UsableFraction: 5, MinTabs: -3, MaxTabs: -1, TabsPerBrowser: -2},
			browsers: 3, tabs: 14,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Size(c.snap, c.opts)
			if got.Browsers != c.browsers || got.Tabs != c.tabs {
				t.Fatalf("Size = %d browsers / %d tabs, want %d / %d\nreason: %s", got.Browsers, got.Tabs, c.browsers, c.tabs, got.Reason)
			}
			if c.reasonHas != "" && !strings.Contains(got.Reason, c.reasonHas) {
				t.Fatalf("reason %q does not mention %q", got.Reason, c.reasonHas)
			}
			if got.Reason == "" || strings.ContainsAny(got.Reason, "\n\r") {
				t.Fatalf("reason must be one line, got %q", got.Reason)
			}
		})
	}
}

func TestSizeWithNoInformationAtAll(t *testing.T) {
	got := Size(cgroup.Snapshot{}, SizingOptions{})
	if got.Tabs < DefaultMinTabs || got.Tabs > DefaultMaxTabs || got.Browsers < 1 || got.Browsers > DefaultMaxBrowsers {
		t.Fatalf("Size = %+v out of the documented bounds", got)
	}
	// Documented fallback: runtime.NumCPU cpus and 4 GiB.
	want := Size(cgroup.Snapshot{}, SizingOptions{HostCPUs: runtime.NumCPU(), HostMemBytes: DefaultHostMemBytes})
	if got.Tabs != want.Tabs || got.Browsers != want.Browsers {
		t.Fatalf("fallback = %+v, want the NumCPU/4GiB answer %+v", got, want)
	}
	if !strings.Contains(got.Reason, "no quota") || !strings.Contains(got.Reason, "host unknown") {
		t.Fatalf("reason %q does not say what it assumed", got.Reason)
	}
}

func TestSizeIsMonotonicInResources(t *testing.T) {
	// More CPU or memory never yields fewer tabs, and tabs and browsers stay in
	// their bounds. Cheap property check over a grid.
	prevByCPU := map[uint64]int{}
	for _, mem := range []uint64{1 * gib, 2 * gib, 4 * gib, 6 * gib, 8 * gib, 16 * gib, 64 * gib} {
		prev := 0
		for _, cpu := range []float64{0.5, 1, 2, 4, 8, 16, 64} {
			s := Size(cgroup.Snapshot{CPUQuotaCores: f64(cpu), MemoryLimitBytes: u64(mem)}, SizingOptions{})
			if s.Tabs < prev {
				t.Fatalf("mem %d: tabs fell from %d to %d when cpu rose to %v", mem, prev, s.Tabs, cpu)
			}
			if s.Tabs < DefaultMinTabs || s.Tabs > DefaultMaxTabs || s.Browsers < 1 || s.Browsers > DefaultMaxBrowsers {
				t.Fatalf("out of bounds: %+v", s)
			}
			prev = s.Tabs
			if p, ok := prevByCPU[uint64(cpu*10)]; ok && s.Tabs < p {
				t.Fatalf("cpu %v: tabs fell from %d to %d when memory rose to %d", cpu, p, s.Tabs, mem)
			}
			prevByCPU[uint64(cpu*10)] = s.Tabs
		}
	}
}

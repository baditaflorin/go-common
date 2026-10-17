package chromeguard

import (
	"fmt"
	"math"
	"runtime"

	"github.com/baditaflorin/go-common/cgroup"
)

// Defaults of SizingOptions.
const (
	DefaultTabsPerCPU       = 2.5
	DefaultUsableFraction   = 0.7
	DefaultBaseBrowserBytes = 200 << 20
	DefaultPerTabBytes      = 250 << 20
	DefaultMinTabs          = 4
	DefaultMaxTabs          = 40
	DefaultTabsPerBrowser   = 6
	DefaultMaxBrowsers      = 6
	// DefaultHostMemBytes is assumed when the container has no memory limit and
	// the caller did not say how much the host has.
	DefaultHostMemBytes = 4 << 30
)

const mib = 1 << 20

// SizingOptions tunes Size. Zero fields select the defaults.
type SizingOptions struct {
	// TabsPerCPU is how many concurrent tabs one CPU core sustains (default 2.5,
	// from the js-proxy load test, where CPU saturated first).
	TabsPerCPU float64
	// UsableFraction is the part of the memory limit the browsers may use, the
	// rest being headroom for the Go process, page cache and spikes (default 0.7).
	UsableFraction float64
	// BaseBrowserBytes is the idle footprint of one browser process tree
	// (browser, GPU, network service; default 200 MiB).
	BaseBrowserBytes uint64
	// PerTabBytes is the working memory budgeted for one tab (default 250 MiB).
	PerTabBytes uint64

	MinTabs        int // lower clamp for tabs (default 4)
	MaxTabs        int // upper clamp for tabs (default 40)
	TabsPerBrowser int // tabs one browser serves before another is added (default 6)
	MaxBrowsers    int // upper clamp for browsers (default 6)

	// HostCPUs and HostMemBytes stand in for a limit the cgroup does not set
	// (nil quota, nil memory limit). HostCPUs defaults to runtime.NumCPU() and
	// HostMemBytes to DefaultHostMemBytes. A cgroup quota above HostCPUs is
	// clamped to it when HostCPUs is given.
	HostCPUs     int
	HostMemBytes uint64
}

// Sizing is the concurrency Size derived.
type Sizing struct {
	// Browsers is how many browser processes to run, Tabs how many renders to
	// allow in flight across all of them. Tabs can exceed Browsers times
	// TabsPerBrowser when MaxBrowsers caps the browser count.
	Browsers int
	Tabs     int
	// Reason is a one-line explanation for the start-up log.
	Reason string
}

func (o SizingOptions) withDefaults() SizingOptions {
	if o.TabsPerCPU <= 0 {
		o.TabsPerCPU = DefaultTabsPerCPU
	}
	if o.UsableFraction <= 0 || o.UsableFraction > 1 {
		o.UsableFraction = DefaultUsableFraction
	}
	if o.BaseBrowserBytes == 0 {
		o.BaseBrowserBytes = DefaultBaseBrowserBytes
	}
	if o.PerTabBytes == 0 {
		o.PerTabBytes = DefaultPerTabBytes
	}
	if o.MinTabs <= 0 {
		o.MinTabs = DefaultMinTabs
	}
	if o.MaxTabs <= 0 {
		o.MaxTabs = DefaultMaxTabs
	}
	if o.MaxTabs < o.MinTabs {
		o.MaxTabs = o.MinTabs
	}
	if o.TabsPerBrowser <= 0 {
		o.TabsPerBrowser = DefaultTabsPerBrowser
	}
	if o.MaxBrowsers <= 0 {
		o.MaxBrowsers = DefaultMaxBrowsers
	}
	return o
}

// Size derives how many browsers and tabs a container can run from its cgroup
// limits. It is a pure function of its arguments.
//
//	tabs     = clamp(min(CPU*TabsPerCPU, (Mem*UsableFraction - Base*browsers) / PerTab), MinTabs, MaxTabs)
//	browsers = clamp(ceil(tabs / TabsPerBrowser), 1, MaxBrowsers)
//
// The two depend on each other (more tabs need more browsers, each browser
// costs memory), so Size takes the largest tab count whose memory bill fits.
// When even MinTabs does not fit it still returns MinTabs, and Reason says the
// memory limit is too small. A nil CPU quota or memory limit (unlimited) falls
// back to opts.HostCPUs and opts.HostMemBytes, then to runtime.NumCPU() and
// DefaultHostMemBytes.
func Size(snap cgroup.Snapshot, opts SizingOptions) Sizing {
	o := opts.withDefaults()

	cpus, cpuSrc := 0.0, ""
	switch {
	case snap.CPUQuotaCores != nil && *snap.CPUQuotaCores > 0:
		cpus, cpuSrc = *snap.CPUQuotaCores, "cgroup quota"
		if o.HostCPUs > 0 && cpus > float64(o.HostCPUs) {
			cpus, cpuSrc = float64(o.HostCPUs), "host cpus (quota is higher)"
		}
	case o.HostCPUs > 0:
		cpus, cpuSrc = float64(o.HostCPUs), "host cpus (no quota)"
	default:
		cpus, cpuSrc = float64(runtime.NumCPU()), "runtime.NumCPU (no quota)"
	}

	mem, memSrc := uint64(0), ""
	switch {
	case snap.MemoryLimitBytes != nil && *snap.MemoryLimitBytes > 0:
		mem, memSrc = *snap.MemoryLimitBytes, "cgroup limit"
		if o.HostMemBytes > 0 && mem > o.HostMemBytes {
			mem, memSrc = o.HostMemBytes, "host memory (limit is higher)"
		}
	case o.HostMemBytes > 0:
		mem, memSrc = o.HostMemBytes, "host memory (no limit)"
	default:
		mem, memSrc = DefaultHostMemBytes, "default memory (no limit, host unknown)"
	}

	cpuTabs := int(math.Floor(cpus * o.TabsPerCPU))
	ceiling := min(max(cpuTabs, o.MinTabs), o.MaxTabs)
	usable := float64(mem) * o.UsableFraction

	tabs, browsers, fits := o.MinTabs, 1, false
	for t := ceiling; t >= o.MinTabs; t-- {
		b := browsersFor(t, o)
		if float64(b)*float64(o.BaseBrowserBytes)+float64(t)*float64(o.PerTabBytes) <= usable {
			tabs, browsers, fits = t, b, true
			break
		}
	}
	if !fits {
		browsers = browsersFor(tabs, o)
	}

	var limit string
	switch {
	case !fits:
		limit = fmt.Sprintf("memory too small even for the minimum of %d tabs: raise the limit", o.MinTabs)
	case tabs < ceiling:
		limit = "limited by memory"
	case ceiling == o.MaxTabs && cpuTabs > o.MaxTabs:
		limit = "limited by MaxTabs"
	case ceiling == o.MinTabs && cpuTabs < o.MinTabs:
		limit = "raised to MinTabs"
	default:
		limit = "limited by cpu"
	}
	reason := fmt.Sprintf("%.1f cpus (%s) x %.1f tabs = %d; %.2f GiB (%s) x %.2f usable, %d MiB per browser + %d MiB per tab; %d tabs on %d browsers, %s",
		cpus, cpuSrc, o.TabsPerCPU, cpuTabs, float64(mem)/(1<<30), memSrc, o.UsableFraction,
		o.BaseBrowserBytes/mib, o.PerTabBytes/mib, tabs, browsers, limit)
	return Sizing{Browsers: browsers, Tabs: tabs, Reason: reason}
}

func browsersFor(tabs int, o SizingOptions) int {
	b := (tabs + o.TabsPerBrowser - 1) / o.TabsPerBrowser
	return min(max(b, 1), o.MaxBrowsers)
}

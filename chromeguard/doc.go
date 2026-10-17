// Package chromeguard keeps a Chromium process tree from taking its container
// down: it reads per-process memory from /proc, raises the OOM score of the
// browser's children so the kernel kills a renderer first, kills a renderer that
// grew past a limit, tells the service when a whole browser grew past another,
// and derives how many browsers and tabs a container can afford from its cgroup
// limits. It has no dependency on chromedp or any browser driver: the service
// hands it the browsers' pids.
//
// # Why this exists
//
// A headless browser fleet fails by memory, not by CPU spikes it can shed: one
// page that allocates without bound grows a renderer to gigabytes, the container
// hits its cgroup limit and the kernel OOM-kills whatever has the highest
// oom_score, which is often the Go service or the browser itself, taking every
// other render in flight with it. The guard makes the cheap process die instead:
// the runaway renderer (that one page fails) rather than the container.
//
// # Reading a tree
//
// Tree returns a browser root and its descendants with their roles (browser,
// renderer, gpu, utility, zygote, crashpad, from the --type= argument), RSS and,
// when smaps_rollup is readable, PSS. Summed RSS counts a shared page (the
// Chromium binary, libraries, shared memory) in every process that maps it, so a
// tree's RSS total is an overestimate; PSS divides shared pages among their
// users and is the number to compare with a memory limit when you can afford the
// read (one small file per process). The difference is large: a live Alpine
// Chromium 142 headless tree with three renderer processes summed to 726 MiB of
// RSS and 142 MiB of PSS. Use TreeRSS for a quick total, and MonitorConfig.UsePSS
// when a BrowserMaxRSS should mean what the tree really holds.
//
// # OOM scores
//
// SetOOMScoreAdj and AdjustRenderers raise oom_score_adj (never lower it; a
// process may always raise its own children's score, lowering below the
// inherited minimum needs CAP_SYS_RESOURCE) of renderer, GPU and utility
// processes to DefaultOOMScoreAdj (800), once per process. Chromium starts its
// renderers at 300 and its GPU and utility processes at 200; the browser and
// the zygote (which the browser cannot do without) are never touched.
//
// # Monitor
//
// Monitor ties it together on a timer: new children get their score raised, a
// renderer above RendererMaxRSS (default 1 GiB) is killed with SIGKILL (that pid
// only, re-checked right before the kill, never the browser, init or the
// calling process), and when a browser's whole tree exceeds BrowserMaxRSS the
// OnBrowserOverLimit callback fires once per episode so the service can drain
// and recycle that browser. Metrics: chromeguard_browser_rss_bytes{browser},
// chromeguard_renderers, chromeguard_renderer_kills_total,
// chromeguard_oom_adjusted_total and chromeguard_scan_errors_total.
//
// # Sizing
//
// Size derives browsers and tabs from a cgroup.Snapshot; see its documentation
// for the formula, and Sizing.Reason for the line to put in the start-up log.
//
// # Platforms
//
// Everything reads a proc filesystem whose root defaults to /proc on Linux and
// to none elsewhere; there the readers return ErrUnsupported and Monitor.Run
// just waits for its context. The root can be given explicitly (tests use a fake
// tree under t.TempDir on any platform).
//
// # Usage
//
//	snap, _ := cgroup.Read()
//	sz := chromeguard.Size(snap, chromeguard.SizingOptions{})
//	log.Info("browser pool sized", "browsers", sz.Browsers, "tabs", sz.Tabs, "why", sz.Reason)
//
//	cfg := chromeguard.DefaultMonitorConfig()
//	cfg.RootPIDs = pool.BrowserPIDs // []int, one per running browser
//	cfg.BrowserMaxRSS = 1500 << 20
//	cfg.OnBrowserOverLimit = func(pid int, rss uint64) { pool.DrainAndRecycle(pid) }
//	cfg.Registerer = promx.Registry()
//	mon, err := chromeguard.NewMonitor(cfg)
//	if err != nil {
//	    return err
//	}
//	go mon.Run(ctx)
package chromeguard

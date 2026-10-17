package chromeguard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// recorder collects monitor callbacks and kills.
type recorder struct {
	mu      sync.Mutex
	killed  []int
	cbKills []uint64
	overs   []uint64
	overPID []int
}

func (r *recorder) kill(pid int) error {
	r.mu.Lock()
	r.killed = append(r.killed, pid)
	r.mu.Unlock()
	return nil
}

func (r *recorder) onKilled(pid int, rss uint64) {
	r.mu.Lock()
	r.cbKills = append(r.cbKills, rss)
	r.mu.Unlock()
}

func (r *recorder) onOver(pid int, rss uint64) {
	r.mu.Lock()
	r.overs = append(r.overs, rss)
	r.overPID = append(r.overPID, pid)
	r.mu.Unlock()
}

func (r *recorder) killedPIDs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.killed...)
}

type monitorFixture struct {
	*Monitor
	root  string
	reg   *prometheus.Registry
	rec   *recorder
	clock *fakeClock
	roots *atomic.Pointer[[]int]
}

func newFixture(t *testing.T, root string, mutate func(*MonitorConfig)) *monitorFixture {
	t.Helper()
	rec := &recorder{}
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	roots := &atomic.Pointer[[]int]{}
	initial := []int{pidBrowser, pidBrowser2}
	roots.Store(&initial)
	reg := prometheus.NewRegistry()
	cfg := DefaultMonitorConfig()
	cfg.RootPIDs = func() []int { return *roots.Load() }
	cfg.ProcRoot = root
	cfg.Registerer = reg
	cfg.Now = clock.Now
	cfg.KillFunc = rec.kill // never a real kill on a fake pid
	cfg.OnRendererKilled = rec.onKilled
	cfg.OnBrowserOverLimit = rec.onOver
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewMonitor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.in.pageSize = testPage
	return &monitorFixture{Monitor: m, root: root, reg: reg, rec: rec, clock: clock, roots: roots}
}

func (f *monitorFixture) setRoots(pids ...int) { f.roots.Store(&pids) }

func metricVal(reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	mfs, err := reg.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			ok := len(got) == len(labels)
			for k, v := range labels {
				ok = ok && got[k] == v
			}
			if !ok {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func (f *monitorFixture) val(name string, labels map[string]string) float64 {
	v, _ := metricVal(f.reg, name, labels)
	return v
}

func setRSS(t *testing.T, root string, pid int, kb uint64) {
	t.Helper()
	pages := strconv.FormatUint(kb*1024/testPage, 10)
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "statm"), []byte("250000 "+pages+" 1000 100 0 5000 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorTickRaisesOOMScoresAndPublishesMetrics(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, func(c *MonitorConfig) { c.RendererMaxRSS = 0 }) // no kills here

	f.tick()
	for _, pid := range []int{pidGPU, pidUtility, pidRendA, pidRendZyg, pidRend2} {
		if got := readOOM(t, root, pid); got != 800 {
			t.Errorf("pid %d oom_score_adj = %d, want 800", pid, got)
		}
	}
	if got := readOOM(t, root, pidBrowser); got != 0 {
		t.Errorf("browser oom_score_adj = %d, must stay", got)
	}
	if got := readOOM(t, root, pidZygote); got != 300 {
		t.Errorf("zygote oom_score_adj = %d, must stay (the browser cannot lose it)", got)
	}
	if got := f.val("chromeguard_oom_adjusted_total", nil); got != 5 {
		t.Errorf("oom_adjusted_total = %v, want 5", got)
	}
	if got := f.val("chromeguard_renderers", nil); got != 3 {
		t.Errorf("renderers = %v, want 3", got)
	}
	tree1 := float64(120_000+8_000+40_000+60_000+50_000+200_000+300_000) * 1024
	if got := f.val("chromeguard_browser_rss_bytes", map[string]string{"browser": "0"}); got != tree1 {
		t.Errorf("browser 0 rss = %v, want %v", got, tree1)
	}
	if got := f.val("chromeguard_browser_rss_bytes", map[string]string{"browser": "1"}); got != float64(110_000+70_000)*1024 {
		t.Errorf("browser 1 rss = %v", got)
	}
	if !f.LastScan().Equal(f.clock.Now()) {
		t.Errorf("LastScan = %v, want the fake clock %v", f.LastScan(), f.clock.Now())
	}

	// A second tick adjusts nothing new.
	f.clock.Advance(5 * time.Second)
	f.tick()
	if got := f.val("chromeguard_oom_adjusted_total", nil); got != 5 {
		t.Errorf("oom_adjusted_total = %v after an idle tick, want 5", got)
	}
	if !f.LastScan().Equal(f.clock.Now()) {
		t.Errorf("LastScan did not advance: %v", f.LastScan())
	}
	if got := f.val("chromeguard_scan_errors_total", nil); got != 0 {
		t.Errorf("scan_errors_total = %v", got)
	}
}

func TestMonitorKillsOnlyRenderersAboveTheLimit(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, func(c *MonitorConfig) { c.RendererMaxRSS = 250_000 * 1024 })

	f.tick()
	if got := f.rec.killedPIDs(); len(got) != 1 || got[0] != pidRendA { // 300 MB renderer; the 200 MB one and the others survive
		t.Fatalf("killed = %v, want [%d]", got, pidRendA)
	}
	if len(f.rec.cbKills) != 1 || f.rec.cbKills[0] != 300_000*1024 {
		t.Fatalf("OnRendererKilled = %v", f.rec.cbKills)
	}
	if got := f.val("chromeguard_renderer_kills_total", nil); got != 1 {
		t.Errorf("kills_total = %v", got)
	}
	// The killed renderer no longer counts toward the tree or the renderer gauge.
	if got := f.val("chromeguard_renderers", nil); got != 2 {
		t.Errorf("renderers = %v, want 2", got)
	}
	want := float64(120_000+8_000+40_000+60_000+50_000+200_000) * 1024
	if got := f.val("chromeguard_browser_rss_bytes", map[string]string{"browser": "0"}); got != want {
		t.Errorf("browser 0 rss = %v, want %v (without the killed renderer)", got, want)
	}
}

func TestMonitorZeroRendererLimitDisablesKills(t *testing.T) {
	root := buildTree(t)
	setRSS(t, root, pidRendA, 50_000_000) // 50 GB
	f := newFixture(t, root, func(c *MonitorConfig) { c.RendererMaxRSS = 0 })
	f.tick()
	if got := f.rec.killedPIDs(); len(got) != 0 {
		t.Fatalf("killed %v with kills disabled", got)
	}
}

func TestDefaultMonitorConfigValues(t *testing.T) {
	c := DefaultMonitorConfig()
	if c.RendererMaxRSS != 1<<30 || c.Interval != 5*time.Second || c.OOMScoreAdj != 800 || c.BrowserMaxRSS != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestMonitorNeverKillsNonRenderersOrItself(t *testing.T) {
	root := buildTree(t)
	// Everything is enormous, the GPU, utility, zygote and the browser included.
	for _, pid := range []int{pidBrowser, pidGPU, pidUtility, pidZygote, pidCrashpad} {
		setRSS(t, root, pid, 50_000_000)
	}
	// A "renderer" that is this very process.
	writeProc(t, root, fakeProc{pid: os.Getpid(), ppid: pidBrowser, comm: "self", args: typed("renderer"), rssKB: 50_000_000})
	f := newFixture(t, root, func(c *MonitorConfig) { c.RendererMaxRSS = 1000 * 1024 })
	f.tick()
	for _, pid := range f.rec.killedPIDs() {
		switch pid {
		case pidBrowser, pidGPU, pidUtility, pidZygote, pidCrashpad, os.Getpid(), 1:
			t.Fatalf("killed protected pid %d", pid)
		}
	}
}

func TestSafeToKillRechecksTheProcess(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, nil)
	sc, _ := f.in.scan()
	procs, _, _ := f.in.treeFrom(sc, pidBrowser, false)
	var renderer Proc
	for _, p := range procs {
		if p.PID == pidRendA {
			renderer = p
		}
	}
	roots := map[int]bool{pidBrowser: true}
	if !f.safeToKill(renderer, roots) {
		t.Fatal("a live renderer was judged unsafe")
	}

	reused := renderer
	reused.StartTicks++ // the pid now belongs to a different process
	if f.safeToKill(reused, roots) {
		t.Fatal("pid reuse was not detected")
	}

	// The process was replaced by something that is not a renderer.
	writeProc(t, root, fakeProc{pid: pidRendA, ppid: pidBrowser, comm: "sleep", args: []string{"sleep", "9"}, rssKB: 1, start: renderer.StartTicks})
	if f.safeToKill(renderer, roots) {
		t.Fatal("a process that stopped being a renderer was judged safe")
	}
	removeProc(t, root, pidRendA)
	if f.safeToKill(renderer, roots) {
		t.Fatal("a vanished process was judged safe")
	}
	if f.safeToKill(Proc{PID: 1, Type: TypeRenderer}, roots) || f.safeToKill(Proc{PID: os.Getpid(), Type: TypeRenderer}, roots) ||
		f.safeToKill(Proc{PID: pidBrowser, Type: TypeRenderer}, roots) || f.safeToKill(Proc{PID: pidGPU, Type: TypeGPU}, roots) {
		t.Fatal("init, self, a watched root or a non-renderer was judged safe")
	}
}

func TestMonitorBrowserOverLimitFiresOncePerEpisode(t *testing.T) {
	root := buildTree(t)
	limit := uint64(1_000_000 * 1024) // 1 GB
	f := newFixture(t, root, func(c *MonitorConfig) {
		c.BrowserMaxRSS = limit
		c.RendererMaxRSS = 0
	})

	f.tick() // tree 0 is 778 MB: under
	if len(f.rec.overs) != 0 {
		t.Fatalf("callback fired under the limit: %v", f.rec.overs)
	}

	setRSS(t, root, pidRendA, 600_000) // tree 0 -> 1.08 GB
	f.tick()
	if len(f.rec.overs) != 1 || f.rec.overPID[0] != pidBrowser || f.rec.overs[0] != uint64(120_000+8_000+40_000+60_000+50_000+200_000+600_000)*1024 {
		t.Fatalf("episode start: %v pids %v", f.rec.overs, f.rec.overPID)
	}
	f.tick()
	setRSS(t, root, pidRendA, 700_000)
	f.tick()
	if len(f.rec.overs) != 1 {
		t.Fatalf("callback fired again inside one episode: %v", f.rec.overs)
	}

	// Falling a little under the limit (not below 90%) is the same episode.
	setRSS(t, root, pidRendA, 500_000) // 978 MB
	f.tick()
	setRSS(t, root, pidRendA, 600_000)
	f.tick()
	if len(f.rec.overs) != 1 {
		t.Fatalf("a dip just under the limit started a new episode: %v", f.rec.overs)
	}

	// Well under: the episode ends. Over again: a new one starts.
	setRSS(t, root, pidRendA, 100_000)
	f.tick()
	setRSS(t, root, pidRendA, 800_000)
	f.tick()
	if len(f.rec.overs) != 2 {
		t.Fatalf("second episode not reported: %v", f.rec.overs)
	}
	if got := f.val("chromeguard_browser_rss_bytes", map[string]string{"browser": "0"}); got < float64(limit) {
		t.Errorf("gauge = %v, want above the limit", got)
	}
}

func TestMonitorEpisodeEndsWhenTheBrowserIsRecycled(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, func(c *MonitorConfig) {
		c.BrowserMaxRSS = 500_000 * 1024
		c.RendererMaxRSS = 0
	})
	f.setRoots(pidBrowser)
	f.tick()
	if len(f.rec.overs) != 1 {
		t.Fatalf("overs = %v", f.rec.overs)
	}
	// The service drains the browser and starts another with a new pid: the new
	// one is over the limit too, and that is a new episode.
	for _, pid := range []int{pidBrowser, pidCrashpad, pidZygote, pidGPU, pidUtility, pidRendA, pidRendZyg} {
		removeProc(t, root, pid)
	}
	writeProc(t, root, fakeProc{pid: 400, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 900_000})
	f.setRoots(400)
	f.tick()
	if len(f.rec.overs) != 2 || f.rec.overPID[1] != 400 {
		t.Fatalf("recycled browser's episode: %v pids %v", f.rec.overs, f.rec.overPID)
	}
}

func TestMonitorHandlesVanishedAndShrinkingBrowsers(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, nil)
	f.tick()
	if _, ok := metricVal(f.reg, "chromeguard_browser_rss_bytes", map[string]string{"browser": "1"}); !ok {
		t.Fatal("browser 1 gauge missing")
	}
	f.setRoots(pidBrowser) // only one browser listed now
	f.tick()
	if _, ok := metricVal(f.reg, "chromeguard_browser_rss_bytes", map[string]string{"browser": "1"}); ok {
		t.Fatal("gauge of a browser that is no longer listed was left behind")
	}
	f.setRoots(pidBrowser, 99999) // a listed browser that does not exist
	f.tick()
	if _, ok := metricVal(f.reg, "chromeguard_browser_rss_bytes", map[string]string{"browser": "1"}); ok {
		t.Fatal("gauge of a vanished browser was left behind")
	}
	if got := f.val("chromeguard_scan_errors_total", nil); got != 0 {
		t.Errorf("a vanished browser counted as a scan error: %v", got)
	}
	f.setRoots() // no browsers at all
	f.tick()
	if got := f.val("chromeguard_renderers", nil); got != 0 {
		t.Errorf("renderers = %v with no browsers", got)
	}
}

func TestMonitorUsePSS(t *testing.T) {
	root := buildTree(t)
	// RSS says the renderer is huge, PSS (its own share) says it is small.
	setRSS(t, root, pidRendA, 900_000)
	writeProc(t, root, fakeProc{pid: pidRendA, ppid: pidBrowser, comm: "chromium", args: typed("renderer"), rssKB: 900_000, pssKB: 100_000, oom: 300})

	rssMon := newFixture(t, root, func(c *MonitorConfig) { c.RendererMaxRSS = 500_000 * 1024 })
	rssMon.tick()
	if got := rssMon.rec.killedPIDs(); len(got) != 1 || got[0] != pidRendA {
		t.Fatalf("RSS mode killed %v, want the renderer", got)
	}

	pssMon := newFixture(t, root, func(c *MonitorConfig) {
		c.RendererMaxRSS = 500_000 * 1024
		c.UsePSS = true
	})
	pssMon.tick()
	if got := pssMon.rec.killedPIDs(); len(got) != 0 {
		t.Fatalf("PSS mode killed %v, but the renderer's PSS is under the limit", got)
	}
	// A process whose smaps_rollup is unreadable falls back to its RSS.
	if err := os.Remove(filepath.Join(root, strconv.Itoa(pidRendA), "smaps_rollup")); err != nil {
		t.Fatal(err)
	}
	pssMon.tick()
	if got := pssMon.rec.killedPIDs(); len(got) != 1 || got[0] != pidRendA {
		t.Fatalf("PSS mode without smaps_rollup killed %v, want the RSS fallback to apply", got)
	}
}

func TestMonitorCountsScanErrors(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "no-such-proc"), nil)
	f.tick()
	f.tick()
	if got := f.val("chromeguard_scan_errors_total", nil); got != 2 {
		t.Fatalf("scan_errors_total = %v, want 2", got)
	}
	if len(f.rec.killedPIDs()) != 0 {
		t.Fatal("killed something without a scan")
	}
}

func TestMonitorKillFailureIsCountedNotFatal(t *testing.T) {
	root := buildTree(t)
	var calls atomic.Int32
	f := newFixture(t, root, func(c *MonitorConfig) {
		c.RendererMaxRSS = 250_000 * 1024
		c.KillFunc = func(pid int) error { calls.Add(1); return errors.New("operation not permitted") }
	})
	f.tick()
	if calls.Load() != 1 {
		t.Fatalf("kill attempts = %d", calls.Load())
	}
	if f.val("chromeguard_renderer_kills_total", nil) != 0 || f.val("chromeguard_scan_errors_total", nil) != 1 {
		t.Fatal("a failed kill was counted as a kill, or not counted as an error")
	}
	if len(f.rec.cbKills) != 0 {
		t.Fatal("OnRendererKilled fired for a kill that failed")
	}
	// A process that already exited is not an error.
	f2 := newFixture(t, root, func(c *MonitorConfig) {
		c.RendererMaxRSS = 250_000 * 1024
		c.KillFunc = func(pid int) error { return os.ErrProcessDone }
		c.Registerer = prometheus.NewRegistry()
	})
	f2.tick()
	if f2.val("chromeguard_scan_errors_total", nil) != 0 {
		t.Fatal("an already-finished process was counted as an error")
	}
}

func TestMonitorKillsARealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sleep")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// A fake tree in which the real sleep process plays a runaway renderer. Only
	// that pid is ever signalled: the browser root is fake and protected.
	root := t.TempDir()
	writeProc(t, root, fakeProc{pid: pidBrowser, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 1000})
	writeProc(t, root, fakeProc{pid: pid, ppid: pidBrowser, comm: "chromium", args: typed("renderer"), rssKB: 3_000_000})
	var killed []uint64
	m, err := NewMonitor(MonitorConfig{
		RootPIDs:         func() []int { return []int{pidBrowser} },
		ProcRoot:         root,
		RendererMaxRSS:   1 << 30,
		OOMScoreAdj:      -1,
		Registerer:       prometheus.NewRegistry(),
		OnRendererKilled: func(p int, rss uint64) { killed = append(killed, rss) },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.in.pageSize = testPage

	m.tick()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.Success() {
			t.Fatalf("sleep exited with %v, want killed by a signal", err)
		}
		if ws := ee.Sys(); ws == nil || ee.ProcessState.ExitCode() != -1 {
			t.Fatalf("exit code = %d, want -1 (signalled)", ee.ProcessState.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runaway renderer was not killed")
	}
	if len(killed) != 1 || killed[0] != 3_000_000*1024 {
		t.Fatalf("OnRendererKilled = %v", killed)
	}
}

func TestMonitorRunTicksUntilCancelled(t *testing.T) {
	root := buildTree(t)
	var scans atomic.Int32
	f := newFixture(t, root, func(c *MonitorConfig) {
		c.Interval = 20 * time.Millisecond
		c.RendererMaxRSS = 0
		c.RootPIDs = func() []int { scans.Add(1); return []int{pidBrowser} }
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for scans.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d scans", scans.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if got := f.val("chromeguard_oom_adjusted_total", nil); got == 0 {
		t.Fatal("Run never adjusted anything")
	}
}

func TestMonitorRunIsIdleWithoutAProcFilesystem(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux has /proc")
	}
	m, err := NewMonitor(MonitorConfig{RootPIDs: func() []int { return []int{1} }, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	m.Run(ctx) // must neither panic nor spin; it returns with the context
}

func TestNewMonitorValidation(t *testing.T) {
	if _, err := NewMonitor(MonitorConfig{Registerer: prometheus.NewRegistry()}); err == nil {
		t.Fatal("NewMonitor without RootPIDs succeeded")
	}
	// A second monitor on the same registry reuses the collectors.
	reg := prometheus.NewRegistry()
	for i := 0; i < 2; i++ {
		if _, err := NewMonitor(MonitorConfig{RootPIDs: func() []int { return nil }, Registerer: reg}); err != nil {
			t.Fatalf("monitor %d: %v", i, err)
		}
	}
}

func TestMonitorRaceUnderConcurrentTicks(t *testing.T) {
	root := buildTree(t)
	f := newFixture(t, root, func(c *MonitorConfig) {
		c.BrowserMaxRSS = 500_000 * 1024
		c.RendererMaxRSS = 250_000 * 1024
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				f.tick()
				_ = f.LastScan()
			}
		}()
	}
	wg.Wait()
}

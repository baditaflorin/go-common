package chromeguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Defaults of MonitorConfig as returned by DefaultMonitorConfig.
const (
	DefaultInterval       = 5 * time.Second
	DefaultRendererMaxRSS = 1 << 30
)

// overLimitReset is the fraction of BrowserMaxRSS a browser must fall below for
// an over-limit episode to end, so a tree hovering at the limit does not fire
// the callback on every crossing.
const overLimitReset = 0.9

// MonitorConfig configures NewMonitor. Start from DefaultMonitorConfig: a
// literal leaves RendererMaxRSS at 0, which disables killing.
type MonitorConfig struct {
	// RootPIDs returns the pids of the browser (main) processes to watch, in a
	// stable order: the position is the browser index used in metrics. It is
	// called on every tick, so it may track browsers that start and stop.
	RootPIDs func() []int

	// Interval between scans (default DefaultInterval, 5 s). A scan reads
	// /proc/<pid>/stat of every process in the pid namespace and the details
	// of each watched tree.
	Interval time.Duration

	// RendererMaxRSS kills (SIGKILL, that one pid) any --type=renderer process
	// whose memory exceeds it. 0 disables. DefaultMonitorConfig sets 1 GiB.
	// The browser survives and the page's render fails, which beats the kernel
	// OOM-killing the whole container.
	RendererMaxRSS uint64

	// BrowserMaxRSS is the limit for a whole browser tree. It never kills:
	// OnBrowserOverLimit fires once per episode so the service can drain that
	// browser and start a fresh one. 0 disables.
	BrowserMaxRSS uint64

	// OOMScoreAdj is raised on renderer, GPU and utility children the
	// first time they are seen (0 selects DefaultOOMScoreAdj, 800; a negative
	// value turns the adjustment off).
	OOMScoreAdj int

	// UsePSS compares PSS instead of RSS against both limits and reports it in
	// the gauge, falling back to RSS for a process whose smaps_rollup cannot be
	// read. RSS counts shared pages in every process that maps them, so a tree's
	// summed RSS overstates its real use; PSS does not, at the price of reading
	// smaps_rollup for every process each tick.
	UsePSS bool

	// OnBrowserOverLimit is called with the browser's root pid and its tree
	// memory when the tree first exceeds BrowserMaxRSS. OnRendererKilled is
	// called after a renderer was killed. Both run on the monitor's goroutine
	// and must return quickly (hand work to another goroutine).
	OnBrowserOverLimit func(pid int, rss uint64)
	OnRendererKilled   func(pid int, rss uint64)

	// Now is the clock for LastScan and log rate limiting (default time.Now).
	Now func() time.Time
	// ProcRoot is the proc filesystem to read ("" selects /proc on Linux). On
	// other platforms the default is none and Run does nothing.
	ProcRoot string
	// KillFunc terminates a process (default: SIGKILL the pid). Tests replace
	// it.
	KillFunc func(pid int) error

	// Registerer receives the chromeguard_* metrics (default promx.Registry()).
	Registerer prometheus.Registerer
	// Logger receives operational logs (default slog.Default()).
	Logger *slog.Logger
}

// DefaultMonitorConfig returns the documented defaults: a 5 s interval,
// renderers killed above 1 GiB, OOM score 800. The caller sets RootPIDs and
// the callbacks, and BrowserMaxRSS if wanted.
func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{
		Interval:       DefaultInterval,
		RendererMaxRSS: DefaultRendererMaxRSS,
		OOMScoreAdj:    DefaultOOMScoreAdj,
	}
}

// Monitor watches Chromium process trees, raises the OOM score of their
// children, kills renderers that grew past a limit and reports trees that grew
// past another. Create it with NewMonitor and run it with Run.
type Monitor struct {
	cfg MonitorConfig
	in  *Inspector
	m   *metrics
	log *slog.Logger

	mu       sync.Mutex
	over     map[int]bool // root pid -> in an over-limit episode
	lastScan time.Time
	exported int // number of browser gauge labels currently exported
	warned   map[string]time.Time
}

// NewMonitor validates cfg and registers the metrics.
func NewMonitor(cfg MonitorConfig) (*Monitor, error) {
	if cfg.RootPIDs == nil {
		return nil, fmt.Errorf("chromeguard: MonitorConfig.RootPIDs is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.KillFunc == nil {
		cfg.KillFunc = killProcess
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m, err := newMetrics(cfg.Registerer)
	if err != nil {
		return nil, err
	}
	return &Monitor{
		cfg:    cfg,
		in:     NewInspector(cfg.ProcRoot),
		m:      m,
		log:    cfg.Logger.With("component", "chromeguard"),
		over:   make(map[int]bool),
		warned: make(map[string]time.Time),
	}, nil
}

// killProcess sends SIGKILL to exactly one pid.
func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// LastScan is when the last scan finished (zero before the first). A service
// can report a stale value in its health check.
func (m *Monitor) LastScan() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastScan
}

// Run scans every Interval until ctx is cancelled, starting immediately. Where
// there is no proc filesystem to read (non-Linux, no ProcRoot) it logs once and
// just waits for ctx, so callers need no platform check.
func (m *Monitor) Run(ctx context.Context) {
	if !m.in.supported() {
		m.log.Info("no proc filesystem on this platform, monitor idle")
		<-ctx.Done()
		return
	}
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		m.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// metric picks the number the limits are compared against.
func (m *Monitor) metric(p Proc) uint64 {
	if m.cfg.UsePSS && p.PSSKnown {
		return p.PSSBytes
	}
	return p.RSSBytes
}

// warn logs at most once per key per minute.
func (m *Monitor) warn(key, msg string, args ...any) {
	now := m.cfg.Now()
	m.mu.Lock()
	last, seen := m.warned[key]
	ok := !seen || now.Sub(last) >= time.Minute
	if ok {
		m.warned[key] = now
	}
	m.mu.Unlock()
	if ok {
		m.log.Warn(msg, args...)
	}
}

// tick is one scan: adjust new children, enforce the renderer limit, publish
// tree sizes and fire the browser callback.
func (m *Monitor) tick() {
	defer func() {
		m.mu.Lock()
		m.lastScan = m.cfg.Now()
		m.mu.Unlock()
	}()

	roots := m.cfg.RootPIDs()
	sc, err := m.in.scan()
	if err != nil {
		m.m.scanErrors.Inc()
		m.warn("scan", "scanning the proc filesystem failed", "error", err.Error())
		return
	}
	m.in.pruneTouched(sc)

	isRoot := make(map[int]bool, len(roots))
	for _, r := range roots {
		isRoot[r] = true
	}

	renderers := 0
	liveOver := make(map[int]bool)
	for idx, root := range roots {
		procs, readErrs, err := m.in.treeFrom(sc, root, m.cfg.UsePSS)
		if err != nil {
			// The browser is gone (crashed, or being recycled): nothing to read.
			m.exportRSS(idx, 0, false)
			continue
		}
		if readErrs > 0 {
			m.m.scanErrors.Add(float64(readErrs))
			m.warn("read", "some /proc reads failed", "browser", idx, "failed", readErrs)
		}

		if m.cfg.OOMScoreAdj >= 0 {
			raised, failed, firstErr := m.in.adjustProcs(procs, m.cfg.OOMScoreAdj)
			m.m.oomAdj.Add(float64(raised))
			if failed > 0 {
				m.m.scanErrors.Add(float64(failed))
				m.warn("oomadj", "could not raise oom_score_adj", "error", firstErr.Error())
			}
		}

		var killed uint64
		for _, p := range procs {
			if p.Type != TypeRenderer {
				continue
			}
			rss := m.metric(p)
			if m.cfg.RendererMaxRSS > 0 && rss > m.cfg.RendererMaxRSS && m.safeToKill(p, isRoot) {
				if m.killRenderer(p, rss) {
					killed += rss
					continue
				}
			}
			renderers++
		}

		var total uint64
		for _, p := range procs {
			total += m.metric(p)
		}
		total -= min(killed, total)
		m.exportRSS(idx, total, true)

		if m.cfg.BrowserMaxRSS > 0 {
			m.mu.Lock()
			was := m.over[root]
			now := was
			switch {
			case total > m.cfg.BrowserMaxRSS:
				now = true
			case float64(total) < overLimitReset*float64(m.cfg.BrowserMaxRSS):
				now = false
			}
			m.mu.Unlock()
			liveOver[root] = now
			if now && !was && m.cfg.OnBrowserOverLimit != nil {
				m.log.Warn("browser memory over limit", "browser", idx, "pid", root, "rss_bytes", total, "limit_bytes", m.cfg.BrowserMaxRSS)
				m.cfg.OnBrowserOverLimit(root, total)
			}
		}
	}
	m.m.renderers.Set(float64(renderers))
	m.trimExport(len(roots))

	m.mu.Lock()
	m.over = liveOver // episodes of browsers no longer listed end with them
	m.mu.Unlock()
}

// safeToKill re-reads the process right before the kill: it must still be the
// same process (pid reuse), still a renderer, and never the monitor's own
// process, init or a watched browser.
func (m *Monitor) safeToKill(p Proc, isRoot map[int]bool) bool {
	if p.PID <= 1 || p.PID == os.Getpid() || isRoot[p.PID] || p.Type != TypeRenderer {
		return false
	}
	sc, err := m.in.scan()
	if err != nil {
		return false
	}
	if sc.start[p.PID] != p.StartTicks {
		return false
	}
	cur, _, alive := m.in.readProc(sc, p.PID, false, false)
	return alive && cur.Type == TypeRenderer
}

// killRenderer SIGKILLs one renderer and reports whether it did.
func (m *Monitor) killRenderer(p Proc, rss uint64) bool {
	if err := m.cfg.KillFunc(p.PID); err != nil {
		if !isGone(err) {
			m.m.scanErrors.Inc()
			m.warn("kill", "could not kill runaway renderer", "pid", p.PID, "error", err.Error())
		}
		return false
	}
	m.m.kills.Inc()
	m.log.Warn("killed runaway renderer", "pid", p.PID, "ppid", p.PPID, "rss_bytes", rss, "limit_bytes", m.cfg.RendererMaxRSS)
	if m.cfg.OnRendererKilled != nil {
		m.cfg.OnRendererKilled(p.PID, rss)
	}
	return true
}

// isGone reports a kill that failed because the process was already finished.
func isGone(err error) bool { return errors.Is(err, os.ErrProcessDone) }

// exportRSS sets (or clears) the gauge for one browser index.
func (m *Monitor) exportRSS(idx int, bytes uint64, present bool) {
	if idx >= maxBrowserLabels {
		return
	}
	if !present {
		m.m.browserRSS.DeleteLabelValues(browserLabel(idx))
		return
	}
	m.m.browserRSS.WithLabelValues(browserLabel(idx)).Set(float64(bytes))
	m.mu.Lock()
	m.exported = max(m.exported, idx+1)
	m.mu.Unlock()
}

// trimExport removes the gauges of browser indexes that no longer exist.
func (m *Monitor) trimExport(n int) {
	m.mu.Lock()
	prev := m.exported
	m.exported = min(n, maxBrowserLabels)
	m.mu.Unlock()
	for i := n; i < prev && i < maxBrowserLabels; i++ {
		m.m.browserRSS.DeleteLabelValues(browserLabel(i))
	}
}

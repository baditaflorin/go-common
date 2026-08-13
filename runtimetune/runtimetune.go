// Package runtimetune applies conservative, container-aware Go runtime
// memory/GC tuning at process start.
//
// # Why this exists
//
// Every fleet service runs in a container with a cgroup memory limit, but
// nothing in go-common ever told the Go runtime about it. Two concrete
// pathologies followed:
//
//  1. **Hard OOM kills.** Explicit `mem_limit`s across the fleet sum to
//     ~269 GB against 47 GB physical (5.7x oversubscribed), so the limit is
//     an accounting fiction right up until a service actually reaches it —
//     at which point the kernel SIGKILLs it (observed:
//     `trl-biz-classifier-pr18` exited 137). The Go runtime had no idea a
//     ceiling existed, so it never GC'd harder on the way up. GOMEMLIMIT
//     converts that hard kill into GC back-pressure.
//
//  2. **GC thrash on tiny heaps.** With the default GOGC=100, the next-GC
//     target is 2x the live heap — an *absolute* target that shrinks with
//     the heap. A service with an 11 MB live heap therefore collects every
//     11 MB of allocation, forever. Measured worst case: `fleet-discovery`
//     had run 550,741 GC cycles totalling 15,460 s of CPU — 23% of its
//     entire CPU budget — on an 11 MB heap. It spent a quarter of its CPU
//     collecting garbage it did not have.
//
// # The two knobs pull in opposite directions — read this before changing defaults
//
// For a *tiny* heap, GC is too frequent and the fix is to RAISE GOGC.
// For a *large* heap, peak RSS is the risk and the fix is to LOWER GOGC.
// A single static GOGC cannot serve both, and given 5.7x memory
// oversubscription a fleet-wide GOGC bump is actively dangerous: it would
// let all ~300 services balloon at once.
//
// So this package does not raise GOGC statically. It maintains an
// **absolute minimum heap target** (default 32 MiB) by recomputing GOGC
// from the live heap:
//
//	GOGC = clamp((MinHeapTarget/liveHeap - 1) * 100, BaseGOGC, MaxGOGC)
//
// The consequences are deliberately asymmetric:
//
//   - Live heap >= MinHeapTarget → GOGC stays at BaseGOGC (100). Services
//     with real heaps see *no change at all*: same GC behaviour, same RSS.
//   - Live heap < MinHeapTarget → GOGC rises just enough to push the next-GC
//     target up to the floor. fleet-discovery's 11 MB heap yields GOGC≈190
//     at the 32 MiB default (≈2x fewer cycles), and operators can raise the
//     floor per-service for the pathological cases.
//
// The extra RSS is therefore bounded, per service, by MinHeapTarget — and
// only for services that were thrashing in the first place. That bound is
// why the default is a modest 32 MiB rather than the ~64-128 MiB that would
// fully flatten fleet-discovery's GC cost: 300 services x an unbounded
// floor does not fit in 47 GB. Raise it per-service, not fleet-wide.
//
// GOMEMLIMIT remains the backstop above all of this: the runtime's real
// next-GC target is the MINIMUM of the GOGC-derived target and whatever the
// memory limit permits, so raising GOGC can never push a service past its
// container limit.
//
// # Operator overrides always win
//
// If the `GOGC` or `GOMEMLIMIT` environment variables are set, the Go
// runtime has already applied them at startup and this package does not
// touch that knob — including not starting the GOGC tuner, which would
// otherwise stomp an operator-pinned GOGC. The two knobs are considered
// independently: pinning GOGC does not disable GOMEMLIMIT detection.
//
// # Kill switch
//
//	FLEET_RUNTIME_TUNING=off
//
// disables everything in this package for a single service, no rollback
// needed. An unrecognised value also disables it (fail back to the
// pre-existing, untuned behaviour).
//
// # Usage
//
// server.New calls Apply automatically, so every fleet service picks this
// up on its next rebuild. Non-server binaries (workers, cron jobs) can call
// runtimetune.Apply() directly as the first statement in main.
package runtimetune

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Environment variables read by ConfigFromEnv.
const (
	// EnvKillSwitch disables all tuning when set to a falsey value (or to
	// anything unrecognised). Unset means enabled.
	EnvKillSwitch = "FLEET_RUNTIME_TUNING"
	// EnvMemLimitRatio overrides the fraction of the cgroup limit used as
	// GOMEMLIMIT (default DefaultMemLimitRatio).
	EnvMemLimitRatio = "FLEET_RUNTIME_MEMLIMIT_RATIO"
	// EnvMinHeapBytes overrides the absolute minimum heap target that the
	// GOGC floor aims for. 0 disables the GOGC floor entirely, leaving
	// GOGC untouched.
	EnvMinHeapBytes = "FLEET_RUNTIME_MIN_HEAP_BYTES"
	// EnvBaseGOGC overrides the GOGC used once the live heap is at or above
	// the minimum heap target (default DefaultBaseGOGC — i.e. stock Go).
	EnvBaseGOGC = "FLEET_RUNTIME_GOGC_BASE"
	// EnvMaxGOGC caps how high the floor logic may push GOGC.
	EnvMaxGOGC = "FLEET_RUNTIME_GOGC_MAX"
	// EnvInterval overrides how often the GOGC floor is recomputed.
	EnvInterval = "FLEET_RUNTIME_TUNING_INTERVAL"

	// EnvGOGC and EnvGOMEMLIMIT are the stock Go runtime knobs. When either
	// is set by the operator, this package defers on that knob.
	EnvGOGC       = "GOGC"
	EnvGOMEMLIMIT = "GOMEMLIMIT"
)

// Defaults. See the package doc for the reasoning behind each value.
const (
	// DefaultMemLimitRatio is the fraction of the detected cgroup memory
	// limit installed as GOMEMLIMIT. The 20% headroom covers everything the
	// Go heap accounting does NOT include but the cgroup does: thread
	// stacks beyond the runtime's estimate, cgo/malloc arenas, mmap'd
	// files, and the page cache charged to the container.
	DefaultMemLimitRatio = 0.8

	// DefaultMinHeapTarget is the absolute floor for the next-GC target.
	// 32 MiB is deliberately modest: the cost is bounded per service, but
	// it is paid across ~300 services on a 47 GB host.
	DefaultMinHeapTarget int64 = 32 << 20

	// DefaultBaseGOGC is stock Go. Services with a live heap at or above
	// DefaultMinHeapTarget run exactly as they do today.
	DefaultBaseGOGC = 100

	// DefaultMaxGOGC caps the floor logic. Without a cap, a 1 MB heap and a
	// 32 MiB floor would ask for GOGC=3100; the cap keeps the blast radius
	// of a mis-set floor finite.
	DefaultMaxGOGC = 800

	// DefaultInterval is how often the GOGC floor is recomputed. Each
	// recomputation costs one cheap runtime/metrics read; only an actual
	// change costs a (brief) stop-the-world.
	DefaultInterval = 30 * time.Second

	// MinMemoryLimitBytes is the smallest GOMEMLIMIT this package will
	// install. Below roughly this size the runtime spends its life in
	// back-to-back GCs trying to respect a limit it cannot meet, which is
	// strictly worse than no limit at all. Containers smaller than
	// MinMemoryLimitBytes/DefaultMemLimitRatio are left untuned.
	MinMemoryLimitBytes int64 = 32 << 20

	// MaxCredibleCgroupLimit is the point above which a reported cgroup
	// limit is treated as "no limit". cgroup v1 reports unlimited as a
	// near-int64-max sentinel (9223372036854771712 and variants); no real
	// container has 128 TiB.
	MaxCredibleCgroupLimit int64 = 1 << 47
)

// Canonical cgroup memory-limit paths.
const (
	CgroupV2Path = "/sys/fs/cgroup/memory.max"
	CgroupV1Path = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// Cgroup limit sources reported in Result.CgroupSource.
const (
	SourceNone = "none"
	SourceV2   = "cgroupv2"
	SourceV1   = "cgroupv1"
)

// Who set a given knob, reported in Result.
const (
	SetByNone     = "none"
	SetByOperator = "operator"
	SetByTuner    = "runtimetune"
)

// Config is the fully-resolved tuning configuration. Use ConfigFromEnv to
// build one from the environment, or DefaultConfig for the defaults.
type Config struct {
	// Enabled is the kill switch. False means this package does nothing.
	Enabled bool
	// MemLimitRatio is the fraction of the cgroup limit installed as
	// GOMEMLIMIT. Values outside (0,1) fall back to DefaultMemLimitRatio.
	MemLimitRatio float64
	// MinHeapTarget is the absolute floor for the next-GC target.
	// <= 0 disables the GOGC floor (GOGC is left entirely alone).
	MinHeapTarget int64
	// BaseGOGC is the GOGC applied once the live heap reaches MinHeapTarget.
	BaseGOGC int
	// MaxGOGC caps the floor logic.
	MaxGOGC int
	// Interval is how often the GOGC floor is recomputed.
	Interval time.Duration
	// CgroupV2Path / CgroupV1Path are overridable for tests. Empty means
	// "skip this source".
	CgroupV2Path string
	CgroupV1Path string
}

// Result describes what Apply actually did. Every field is safe to log.
type Result struct {
	// Enabled is false when the kill switch turned everything off.
	Enabled bool
	// Skipped, when non-empty, explains why nothing (or nothing more) was
	// applied.
	Skipped string
	// CgroupLimitBytes is the detected container memory limit, or 0 when
	// none could be detected.
	CgroupLimitBytes int64
	// CgroupSource is one of SourceNone / SourceV2 / SourceV1.
	CgroupSource string
	// MemoryLimitBytes is the GOMEMLIMIT now in effect (0 when unset by us
	// and not detectable).
	MemoryLimitBytes int64
	// MemoryLimitSetBy is SetByNone, SetByOperator or SetByTuner.
	MemoryLimitSetBy string
	// GOGCApplied is the GOGC value installed at startup (0 when untouched).
	GOGCApplied int
	// GOGCSetBy is SetByNone, SetByOperator or SetByTuner.
	GOGCSetBy string
	// MinHeapTarget is the effective floor after clamping against the
	// memory limit. 0 when the GOGC floor is inactive.
	MinHeapTarget int64
	// Interval is the floor's recomputation period (0 when inactive).
	Interval time.Duration
}

// LogLine renders the result as a single, greppable line.
func (r Result) LogLine() string {
	if !r.Enabled {
		return fmt.Sprintf("runtimetune: disabled (%s)", r.Skipped)
	}
	parts := []string{
		"cgroup_source=" + r.CgroupSource,
		"cgroup_limit=" + bytesOrNone(r.CgroupLimitBytes),
		"gomemlimit=" + bytesOrNone(r.MemoryLimitBytes) + " (" + r.MemoryLimitSetBy + ")",
	}
	if r.GOGCSetBy == SetByNone {
		parts = append(parts, "gogc=untouched")
	} else {
		parts = append(parts, fmt.Sprintf("gogc=%d (%s)", r.GOGCApplied, r.GOGCSetBy))
	}
	if r.MinHeapTarget > 0 {
		parts = append(parts,
			"min_heap_target="+bytesOrNone(r.MinHeapTarget),
			"recheck="+r.Interval.String())
	} else {
		parts = append(parts, "min_heap_target=off")
	}
	if r.Skipped != "" {
		parts = append(parts, "note="+strconv.Quote(r.Skipped))
	}
	return "runtimetune: " + strings.Join(parts, " ")
}

func bytesOrNone(n int64) string {
	if n <= 0 {
		return "none"
	}
	if n >= 1<<20 {
		return fmt.Sprintf("%dB(%.1fMiB)", n, float64(n)/(1<<20))
	}
	return fmt.Sprintf("%dB", n)
}

// DefaultConfig returns the fleet defaults with tuning enabled.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		MemLimitRatio: DefaultMemLimitRatio,
		MinHeapTarget: DefaultMinHeapTarget,
		BaseGOGC:      DefaultBaseGOGC,
		MaxGOGC:       DefaultMaxGOGC,
		Interval:      DefaultInterval,
		CgroupV2Path:  CgroupV2Path,
		CgroupV1Path:  CgroupV1Path,
	}
}

// ConfigFromEnv builds a Config from DefaultConfig plus environment
// overrides. Unparseable values fall back to the default (with a warning);
// a bad env var must never take a service down.
func ConfigFromEnv() Config {
	cfg := DefaultConfig()
	cfg.Enabled = killSwitchEnabled(os.Getenv(EnvKillSwitch))

	if v := os.Getenv(EnvMemLimitRatio); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f >= 1 {
			log.Printf("runtimetune: %s=%q is not a ratio in (0,1), using %g",
				EnvMemLimitRatio, v, cfg.MemLimitRatio)
		} else {
			cfg.MemLimitRatio = f
		}
	}
	if v := os.Getenv(EnvMinHeapBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			log.Printf("runtimetune: %s=%q is not a non-negative integer, using %d",
				EnvMinHeapBytes, v, cfg.MinHeapTarget)
		} else {
			cfg.MinHeapTarget = n
		}
	}
	cfg.BaseGOGC = intFromEnv(EnvBaseGOGC, cfg.BaseGOGC)
	cfg.MaxGOGC = intFromEnv(EnvMaxGOGC, cfg.MaxGOGC)
	if v := os.Getenv(EnvInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Printf("runtimetune: %s=%q is not a positive duration, using %s",
				EnvInterval, v, cfg.Interval)
		} else {
			cfg.Interval = d
		}
	}
	return cfg
}

func intFromEnv(name string, defaultVal int) int {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("runtimetune: %s=%q is not a positive integer, using %d", name, v, defaultVal)
		return defaultVal
	}
	return n
}

// killSwitchEnabled interprets FLEET_RUNTIME_TUNING. Unset means enabled.
// Only explicitly truthy values keep it enabled; an unrecognised value
// disables tuning, so a typo fails back to the pre-existing untuned
// behaviour rather than to an unintended new one.
func killSwitchEnabled(v string) bool {
	if v == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		log.Printf("runtimetune: %s=%q is not recognised, disabling tuning", EnvKillSwitch, v)
		return false
	}
}

var (
	applyOnce   sync.Once
	applyDone   atomic.Bool
	applyResult Result
)

// Apply reads the environment, applies the tuning, logs one line
// describing what happened, and returns the result.
//
// It is safe to call from anywhere and any number of times: the tuning is
// applied at most once per process and later calls return the same Result.
func Apply() Result {
	applyOnce.Do(func() {
		applyResult = ApplyConfig(ConfigFromEnv())
		applyDone.Store(true)
		log.Print(applyResult.LogLine())
	})
	return applyResult
}

// Applied reports the process-wide tuning result and whether Apply has run
// at all. Intended for wiring assertions and for services that want to
// surface the tuning decision on a debug endpoint. It never triggers the
// tuning itself.
func Applied() (Result, bool) {
	if !applyDone.Load() {
		return Result{}, false
	}
	return applyResult, true
}

// ApplyConfig applies an explicit configuration. Unlike Apply it does not
// log and is not deduplicated — callers own both. It is exported mainly for
// binaries that configure tuning in code rather than via the environment.
func ApplyConfig(cfg Config) Result {
	return applyConfig(cfg, nil)
}

// applyConfig is the testable core. stop, when non-nil, terminates the
// background GOGC-floor goroutine; a nil stop means "run for the life of
// the process", which is what production wants.
func applyConfig(cfg Config, stop <-chan struct{}) Result {
	cfg = normalize(cfg)

	res := Result{
		Enabled:          cfg.Enabled,
		CgroupSource:     SourceNone,
		MemoryLimitSetBy: SetByNone,
		GOGCSetBy:        SetByNone,
	}
	if !cfg.Enabled {
		res.Skipped = EnvKillSwitch + " is off"
		return res
	}

	// ---- GOMEMLIMIT -----------------------------------------------------
	// An operator-set GOMEMLIMIT has already been applied by the runtime at
	// startup. Read it back (a negative argument to SetMemoryLimit only
	// reads) and defer.
	if envSet(EnvGOMEMLIMIT) {
		res.MemoryLimitSetBy = SetByOperator
		res.MemoryLimitBytes = currentMemoryLimit()
	} else {
		limit, source := detectCgroupLimit(cfg.CgroupV2Path, cfg.CgroupV1Path)
		res.CgroupLimitBytes, res.CgroupSource = limit, source
		if want, ok := memoryLimitFor(limit, cfg.MemLimitRatio, MinMemoryLimitBytes); ok {
			debug.SetMemoryLimit(want)
			res.MemoryLimitBytes = want
			res.MemoryLimitSetBy = SetByTuner
		} else if limit > 0 {
			res.Skipped = appendNote(res.Skipped,
				fmt.Sprintf("cgroup limit %d too small for a safe GOMEMLIMIT", limit))
		} else {
			res.Skipped = appendNote(res.Skipped, "no credible cgroup memory limit")
		}
	}

	// ---- GOGC floor -----------------------------------------------------
	// An operator-pinned GOGC wins outright: we neither set it nor start the
	// tuner that would later overwrite it.
	if envSet(EnvGOGC) {
		res.GOGCSetBy = SetByOperator
		res.GOGCApplied = currentGOGC()
		res.Skipped = appendNote(res.Skipped, "GOGC pinned by operator, floor disabled")
		return res
	}
	if cfg.MinHeapTarget <= 0 {
		res.Skipped = appendNote(res.Skipped, "GOGC floor disabled by config")
		return res
	}

	// Never let the floor fight the container: aiming the next-GC target at
	// more than half the available memory would keep the process
	// permanently pressed against its ceiling. Prefer the installed
	// GOMEMLIMIT as the budget; fall back to the raw cgroup limit, which
	// matters for containers too small to have earned a GOMEMLIMIT.
	budget := res.MemoryLimitBytes
	if budget <= 0 {
		budget = res.CgroupLimitBytes
	}
	floor := effectiveMinHeapTarget(cfg.MinHeapTarget, budget)
	if floor <= 0 {
		res.Skipped = appendNote(res.Skipped, "memory limit too small for a GOGC floor")
		return res
	}

	t := &tuner{
		minHeap:  floor,
		base:     cfg.BaseGOGC,
		max:      cfg.MaxGOGC,
		interval: cfg.Interval,
		readLive: readLiveHeap,
		setGOGC:  debug.SetGCPercent,
	}
	// Establish the current GOGC without permanently mutating it, so the
	// first step can skip a pointless stop-the-world.
	t.current = currentGOGC()
	t.step()

	res.GOGCApplied = t.current
	res.GOGCSetBy = SetByTuner
	res.MinHeapTarget = floor
	res.Interval = cfg.Interval

	go t.run(stop)
	return res
}

// envSet reports whether an operator has meaningfully set name. An empty
// value is treated as unset, matching how the Go runtime itself ignores
// GOGC="" / GOMEMLIMIT="".
func envSet(name string) bool {
	v, ok := os.LookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}

func appendNote(existing, note string) string {
	if existing == "" {
		return note
	}
	return existing + "; " + note
}

// normalize repairs a Config so the rest of the package can assume sane
// values. It never rejects: a bad knob degrades to the default.
func normalize(cfg Config) Config {
	if cfg.MemLimitRatio <= 0 || cfg.MemLimitRatio >= 1 {
		cfg.MemLimitRatio = DefaultMemLimitRatio
	}
	if cfg.BaseGOGC <= 0 {
		cfg.BaseGOGC = DefaultBaseGOGC
	}
	if cfg.MaxGOGC < cfg.BaseGOGC {
		cfg.MaxGOGC = cfg.BaseGOGC
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MinHeapTarget < 0 {
		cfg.MinHeapTarget = 0
	}
	return cfg
}

// ---------------------------------------------------------------------------
// cgroup detection
// ---------------------------------------------------------------------------

// detectCgroupLimit returns the container's memory limit in bytes and which
// source produced it, or (0, SourceNone) when no credible limit exists.
// cgroup v2 is tried first, then v1. An empty path skips that source.
func detectCgroupLimit(v2Path, v1Path string) (int64, string) {
	for _, c := range []struct {
		path   string
		source string
	}{
		{v2Path, SourceV2},
		{v1Path, SourceV1},
	} {
		if c.path == "" {
			continue
		}
		b, err := os.ReadFile(c.path)
		if err != nil {
			continue // missing, unreadable, or a directory — all "no limit here"
		}
		if n, ok := parseCgroupLimit(string(b)); ok {
			return n, c.source
		}
	}
	return 0, SourceNone
}

// parseCgroupLimit parses the body of a cgroup memory-limit file.
//
// It returns ok=false — meaning "treat as unlimited, set nothing" — for the
// cgroup v2 literal "max", the cgroup v1 near-int64-max unlimited sentinel,
// empty or garbage content, zero, and negative values. Anything that is not
// unambiguously a real byte count leaves GOMEMLIMIT alone, because a wrong
// GOMEMLIMIT is far worse than none.
func parseCgroupLimit(content string) (int64, bool) {
	s := strings.TrimSpace(content)
	if s == "" {
		return 0, false
	}
	if strings.EqualFold(s, "max") {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	if n <= 0 {
		return 0, false
	}
	if n >= MaxCredibleCgroupLimit {
		return 0, false // cgroup v1 "unlimited" sentinel, or nonsense
	}
	return n, true
}

// memoryLimitFor turns a detected cgroup limit into the GOMEMLIMIT to
// install. ok=false means "leave GOMEMLIMIT alone".
func memoryLimitFor(cgroupLimit int64, ratio float64, minLimit int64) (int64, bool) {
	if cgroupLimit <= 0 {
		return 0, false
	}
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultMemLimitRatio
	}
	v := int64(float64(cgroupLimit) * ratio)
	if v < minLimit {
		// Too small to be useful: the runtime would GC continuously trying
		// to honour a limit it cannot meet.
		return 0, false
	}
	return v, true
}

// effectiveMinHeapTarget clamps the GOGC floor so it never exceeds half the
// memory budget (the installed GOMEMLIMIT, or the raw cgroup limit when no
// GOMEMLIMIT was installed). A budget of 0 means "unknown", in which case
// the configured floor is used as-is. Returns 0 when the floor is disabled
// or the budget cannot accommodate one.
func effectiveMinHeapTarget(minHeap, budget int64) int64 {
	if minHeap <= 0 {
		return 0
	}
	if budget <= 0 {
		return minHeap
	}
	if half := budget / 2; minHeap > half {
		return half
	}
	return minHeap
}

// ---------------------------------------------------------------------------
// GOGC floor
// ---------------------------------------------------------------------------

type tuner struct {
	minHeap  int64
	base     int
	max      int
	interval time.Duration

	// current is the GOGC this tuner believes is installed. Only the
	// tuner's own goroutine touches it after run starts.
	current int

	readLive func() (int64, bool)
	setGOGC  func(int) int
}

func (t *tuner) run(stop <-chan struct{}) {
	tk := time.NewTicker(t.interval)
	defer tk.Stop()
	for {
		select {
		case <-stop: // a nil stop blocks forever, which is the prod case
			return
		case <-tk.C:
			t.step()
		}
	}
}

// step recomputes the GOGC floor from the current live heap and installs it
// if the change is worth a stop-the-world.
func (t *tuner) step() {
	live, ok := t.readLive()
	if !ok {
		return
	}
	want := gogcForLiveHeap(live, t.minHeap, t.base, t.max)
	if !gogcChangeWorthApplying(t.current, want) {
		return
	}
	t.setGOGC(want)
	t.current = want
}

// gogcForLiveHeap computes the GOGC that puts the next-GC target at
// minHeapTarget, clamped to [base, max].
//
// live <= 0 means no GC has completed yet, so there is no live-heap figure
// to reason about; we return base and wait for real data rather than
// speculatively loosening GC during a service's allocation-heavy startup.
func gogcForLiveHeap(live, minHeapTarget int64, base, max int) int {
	if minHeapTarget <= 0 || live <= 0 {
		return base
	}
	if max < base {
		max = base
	}
	if live >= minHeapTarget {
		return base
	}
	want := (float64(minHeapTarget)/float64(live) - 1) * 100
	if want >= float64(max) {
		return max
	}
	if want <= float64(base) {
		return base
	}
	return int(want)
}

// gogcChangeWorthApplying gates the stop-the-world that debug.SetGCPercent
// costs. Only moves of >= 10% (or any move away from an unknown/negative
// current) are applied, so a slowly-breathing heap does not generate a
// steady drip of STW pauses.
func gogcChangeWorthApplying(cur, want int) bool {
	if cur == want {
		return false
	}
	if cur <= 0 {
		return true
	}
	delta := want - cur
	if delta < 0 {
		delta = -delta
	}
	return delta*10 >= cur
}

// readLiveHeap returns the bytes of live heap as of the last completed GC.
// It uses runtime/metrics rather than runtime.ReadMemStats because the
// latter stops the world; this does not.
func readLiveHeap() (int64, bool) {
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		return 0, false // metric unsupported on this runtime
	}
	v := samples[0].Value.Uint64()
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

// currentMemoryLimit reads GOMEMLIMIT without changing it (a negative
// argument to SetMemoryLimit is documented as read-only).
func currentMemoryLimit() int64 {
	return debug.SetMemoryLimit(-1)
}

// currentGOGC reads GOGC. debug.SetGCPercent has no read-only form — it
// returns the previous value — so this sets and immediately restores.
func currentGOGC() int {
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	return prev
}

package runtimetune

import (
	"bytes"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// TestMain pins the kill switch off for the package so that any incidental
// Apply() (which is guarded by a sync.Once and therefore order-sensitive)
// cannot mutate this test binary's runtime state. Tests that need tuning
// enabled build a Config explicitly or override the env with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(EnvKillSwitch, "off")
	// Keep the warning logs this package emits for bad input out of the
	// test output; individual tests re-enable when they assert on them.
	log.SetOutput(io.Discard)
	code := m.Run()
	log.SetOutput(os.Stderr)
	os.Exit(code)
}

// guardRuntimeState snapshots GOGC and GOMEMLIMIT and restores both when the
// test ends, so no test can leak global runtime state into another.
func guardRuntimeState(t *testing.T) {
	t.Helper()
	prevGOGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(prevGOGC)
	prevLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() {
		debug.SetGCPercent(prevGOGC)
		debug.SetMemoryLimit(prevLimit)
	})
}

// clearRuntimeEnv makes GOGC/GOMEMLIMIT unset for the duration of a test,
// regardless of how the test binary was invoked.
func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvGOGC, EnvGOMEMLIMIT} {
		if v, ok := os.LookupEnv(name); ok {
			os.Unsetenv(name)
			restore := v
			t.Cleanup(func() { os.Setenv(name, restore) })
		}
	}
}

// stopper returns a channel closed at test end, for the tuner goroutine.
func stopper(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	t.Cleanup(func() { close(ch) })
	return ch
}

// ---------------------------------------------------------------------------
// parseCgroupLimit
// ---------------------------------------------------------------------------

func TestParseCgroupLimit(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int64
		wantOK  bool
	}{
		{"v2 max", "max", 0, false},
		{"v2 max newline", "max\n", 0, false},
		{"v2 max uppercase", "MAX\n", 0, false},
		{"v2 max padded", "  max  \n", 0, false},
		{"v2 numeric", "536870912", 536870912, true},
		{"v2 numeric newline", "536870912\n", 536870912, true},
		{"v1 numeric", "2147483648\n", 2147483648, true},
		{"v1 unlimited sentinel", "9223372036854771712\n", 0, false},
		{"v1 unlimited sentinel alt", "9223372036854775807", 0, false},
		{"int64 max", "9223372036854775807\n", 0, false},
		{"just below credible cap", "140737488355327", 140737488355327, true},
		{"at credible cap", "140737488355328", 0, false},
		{"empty", "", 0, false},
		{"whitespace only", "   \n\t ", 0, false},
		{"garbage", "not-a-number\n", 0, false},
		{"partially numeric", "123abc", 0, false},
		{"float", "1073741824.5", 0, false},
		{"hex", "0x40000000", 0, false},
		{"zero", "0\n", 0, false},
		{"negative", "-1\n", 0, false},
		{"negative large", "-1073741824", 0, false},
		{"overflows int64", "99999999999999999999999", 0, false},
		{"padded numeric", "  1073741824  \n", 1073741824, true},
		{"one byte", "1", 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCgroupLimit(tc.content)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseCgroupLimit(%q) = (%d, %v), want (%d, %v)",
					tc.content, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detectCgroupLimit
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDetectCgroupLimit(t *testing.T) {
	dir := t.TempDir()

	v2Numeric := writeFile(t, dir, "v2-numeric", "536870912\n")
	v2Max := writeFile(t, dir, "v2-max", "max\n")
	v2Garbage := writeFile(t, dir, "v2-garbage", "wat\n")
	v1Numeric := writeFile(t, dir, "v1-numeric", "1073741824\n")
	v1Unlimited := writeFile(t, dir, "v1-unlimited", "9223372036854771712\n")
	missing := filepath.Join(dir, "does-not-exist")

	// A directory stands in for "unreadable": os.ReadFile fails on it even
	// as root, unlike a chmod-000 file.
	unreadable := filepath.Join(dir, "unreadable-dir")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name       string
		v2, v1     string
		want       int64
		wantSource string
	}{
		{"v2 wins", v2Numeric, v1Numeric, 536870912, SourceV2},
		{"v2 max falls through to v1", v2Max, v1Numeric, 1073741824, SourceV1},
		{"v2 garbage falls through to v1", v2Garbage, v1Numeric, 1073741824, SourceV1},
		{"v2 missing falls through to v1", missing, v1Numeric, 1073741824, SourceV1},
		{"v2 unreadable falls through to v1", unreadable, v1Numeric, 1073741824, SourceV1},
		{"v1 only", missing, v1Numeric, 1073741824, SourceV1},
		{"both missing", missing, missing, 0, SourceNone},
		{"both unlimited", v2Max, v1Unlimited, 0, SourceNone},
		{"empty paths skipped", "", "", 0, SourceNone},
		{"v2 only, v1 path empty", v2Numeric, "", 536870912, SourceV2},
		{"v1 unlimited, v2 missing", missing, v1Unlimited, 0, SourceNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, source := detectCgroupLimit(tc.v2, tc.v1)
			if got != tc.want || source != tc.wantSource {
				t.Fatalf("detectCgroupLimit = (%d, %s), want (%d, %s)",
					got, source, tc.want, tc.wantSource)
			}
		})
	}
}

// TestDetectCgroupLimitDoesNotTouchHostPaths guards the hermeticity rule:
// the parser must be driven entirely by the configured paths.
func TestDetectCgroupLimitDoesNotTouchHostPaths(t *testing.T) {
	got, source := detectCgroupLimit("", "")
	if got != 0 || source != SourceNone {
		t.Fatalf("empty paths must yield no limit, got (%d, %s)", got, source)
	}
}

// ---------------------------------------------------------------------------
// memoryLimitFor — the fraction math
// ---------------------------------------------------------------------------

func TestMemoryLimitFor(t *testing.T) {
	const (
		mib = 1 << 20
		gib = 1 << 30
	)
	tests := []struct {
		name     string
		limit    int64
		ratio    float64
		minLimit int64
		want     int64
		wantOK   bool
	}{
		{"1 GiB at 0.8", gib, 0.8, MinMemoryLimitBytes, 858993459, true},
		{"512 MiB at 0.8", 512 * mib, 0.8, MinMemoryLimitBytes, 429496729, true},
		{"2 GiB at 0.8", 2 * gib, 0.8, MinMemoryLimitBytes, 1717986918, true},
		{"custom ratio 0.5", gib, 0.5, MinMemoryLimitBytes, gib / 2, true},
		{"custom ratio 0.9", gib, 0.9, MinMemoryLimitBytes, 966367641, true},
		{"ratio 0 falls back to default", gib, 0, MinMemoryLimitBytes, 858993459, true},
		{"ratio 1 falls back to default", gib, 1, MinMemoryLimitBytes, 858993459, true},
		{"ratio >1 falls back to default", gib, 1.5, MinMemoryLimitBytes, 858993459, true},
		{"negative ratio falls back to default", gib, -0.5, MinMemoryLimitBytes, 858993459, true},
		{"no limit detected", 0, 0.8, MinMemoryLimitBytes, 0, false},
		{"negative limit", -1, 0.8, MinMemoryLimitBytes, 0, false},
		// 40 MiB * 0.8 = 32 MiB, exactly the floor — allowed.
		{"exactly at floor", 40 * mib, 0.8, MinMemoryLimitBytes, 32 * mib, true},
		// 39 MiB * 0.8 < 32 MiB — refused rather than set nonsensically small.
		{"just below floor", 39 * mib, 0.8, MinMemoryLimitBytes, 0, false},
		{"tiny container refused", 8 * mib, 0.8, MinMemoryLimitBytes, 0, false},
		{"1 byte container refused", 1, 0.8, MinMemoryLimitBytes, 0, false},
		{"large container", 64 * gib, 0.8, MinMemoryLimitBytes, 54975581388, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := memoryLimitFor(tc.limit, tc.ratio, tc.minLimit)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("memoryLimitFor(%d, %g, %d) = (%d, %v), want (%d, %v)",
					tc.limit, tc.ratio, tc.minLimit, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestMemoryLimitForNeverExceedsCgroupLimit is the safety invariant: the
// installed GOMEMLIMIT must always leave headroom below the cgroup limit,
// for every ratio the config layer can produce.
func TestMemoryLimitForNeverExceedsCgroupLimit(t *testing.T) {
	limits := []int64{40 << 20, 64 << 20, 128 << 20, 512 << 20, 1 << 30, 8 << 30, 64 << 30}
	ratios := []float64{-1, 0, 0.1, 0.5, 0.8, 0.99, 1, 2}
	for _, limit := range limits {
		for _, ratio := range ratios {
			got, ok := memoryLimitFor(limit, ratio, MinMemoryLimitBytes)
			if !ok {
				continue
			}
			if got >= limit {
				t.Fatalf("limit=%d ratio=%g produced GOMEMLIMIT %d >= cgroup limit",
					limit, ratio, got)
			}
			if got < MinMemoryLimitBytes {
				t.Fatalf("limit=%d ratio=%g produced GOMEMLIMIT %d below floor %d",
					limit, ratio, got, MinMemoryLimitBytes)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// effectiveMinHeapTarget
// ---------------------------------------------------------------------------

func TestEffectiveMinHeapTarget(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name    string
		minHeap int64
		budget  int64
		want    int64
	}{
		{"unknown budget passes through", 32 * mib, 0, 32 * mib},
		{"comfortably under half", 32 * mib, 1 << 30, 32 * mib},
		{"exactly half", 32 * mib, 64 * mib, 32 * mib},
		{"clamped to half", 32 * mib, 40 * mib, 20 * mib},
		{"clamped hard", 512 * mib, 64 * mib, 32 * mib},
		{"floor disabled", 0, 1 << 30, 0},
		{"negative floor disabled", -5, 1 << 30, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMinHeapTarget(tc.minHeap, tc.budget); got != tc.want {
				t.Fatalf("effectiveMinHeapTarget(%d, %d) = %d, want %d",
					tc.minHeap, tc.budget, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// gogcForLiveHeap — the GC-frequency decision
// ---------------------------------------------------------------------------

func TestGogcForLiveHeap(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name      string
		live      int64
		floor     int64
		base, max int
		want      int
	}{
		// The fleet-discovery pathology: 11 MB live heap. Default GOGC=100
		// puts the next-GC target at 22 MB; the 32 MiB floor pushes it to
		// GOGC≈190, roughly halving the cycle count.
		{"fleet-discovery 11MB heap", 11 * mib, 32 * mib, 100, 800, 190},
		{"fleet-discovery with 128MiB floor", 11 * mib, 128 * mib, 100, 800, 800},
		{"1MB heap clamped to max", 1 * mib, 32 * mib, 100, 800, 800},
		{"heap exactly at floor", 32 * mib, 32 * mib, 100, 800, 100},
		{"heap above floor untouched", 64 * mib, 32 * mib, 100, 800, 100},
		{"large heap untouched", 4 << 30, 32 * mib, 100, 800, 100},
		{"heap just below floor rounds to base", 31 * mib, 32 * mib, 100, 800, 100},
		{"16MB heap", 16 * mib, 32 * mib, 100, 800, 100},
		{"10MB heap", 10 * mib, 32 * mib, 100, 800, 220},
		{"no GC yet returns base", 0, 32 * mib, 100, 800, 100},
		{"negative live returns base", -1, 32 * mib, 100, 800, 100},
		{"floor disabled returns base", 11 * mib, 0, 100, 800, 100},
		{"negative floor returns base", 11 * mib, -1, 100, 800, 100},
		{"max below base is repaired", 1 * mib, 32 * mib, 100, 50, 100},
		{"custom base honoured", 4 << 30, 32 * mib, 50, 800, 50},
		{"max clamp honoured", 1, 32 * mib, 100, 200, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gogcForLiveHeap(tc.live, tc.floor, tc.base, tc.max)
			if got != tc.want {
				t.Fatalf("gogcForLiveHeap(%d, %d, %d, %d) = %d, want %d",
					tc.live, tc.floor, tc.base, tc.max, got, tc.want)
			}
		})
	}
}

// TestGogcForLiveHeapInvariants asserts the properties that matter more than
// any individual number: the result is always within [base, max], and a heap
// at or above the floor is never given anything but base (so large-heap
// services see zero behaviour change).
func TestGogcForLiveHeapInvariants(t *testing.T) {
	const mib = 1 << 20
	floors := []int64{0, 1, 4 * mib, 32 * mib, 512 * mib}
	lives := []int64{-1, 0, 1, 1024, mib, 11 * mib, 32 * mib, 1 << 30, math.MaxInt64}
	for _, floor := range floors {
		for _, live := range lives {
			got := gogcForLiveHeap(live, floor, 100, 800)
			if got < 100 || got > 800 {
				t.Fatalf("live=%d floor=%d produced out-of-range GOGC %d", live, floor, got)
			}
			if live >= floor && live > 0 && got != 100 {
				t.Fatalf("live=%d >= floor=%d must keep base GOGC, got %d", live, floor, got)
			}
		}
	}
}

func TestGogcChangeWorthApplying(t *testing.T) {
	tests := []struct {
		name      string
		cur, want int
		expect    bool
	}{
		{"identical", 100, 100, false},
		{"tiny increase ignored", 100, 105, false},
		{"tiny decrease ignored", 100, 95, false},
		{"10 percent increase applied", 100, 110, true},
		{"10 percent decrease applied", 100, 90, true},
		{"big jump applied", 100, 800, true},
		{"big drop applied", 800, 100, true},
		{"from GC-off applied", -1, 190, true},
		{"from zero applied", 0, 100, true},
		{"9 percent of 800 ignored", 800, 860, false},
		{"11 percent of 800 applied", 800, 900, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gogcChangeWorthApplying(tc.cur, tc.want); got != tc.expect {
				t.Fatalf("gogcChangeWorthApplying(%d, %d) = %v, want %v",
					tc.cur, tc.want, got, tc.expect)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// tuner.step — with injected live-heap reader and GOGC setter
// ---------------------------------------------------------------------------

func TestTunerStep(t *testing.T) {
	const mib = 1 << 20

	newTuner := func(live int64, ok bool, current int) (*tuner, *[]int) {
		var applied []int
		tu := &tuner{
			minHeap:  32 * mib,
			base:     100,
			max:      800,
			interval: time.Hour,
			current:  current,
			readLive: func() (int64, bool) { return live, ok },
		}
		tu.setGOGC = func(n int) int { applied = append(applied, n); return tu.current }
		return tu, &applied
	}

	t.Run("raises GOGC for a small heap", func(t *testing.T) {
		tu, applied := newTuner(11*mib, true, 100)
		tu.step()
		if len(*applied) != 1 || (*applied)[0] != 190 {
			t.Fatalf("applied = %v, want [190]", *applied)
		}
		if tu.current != 190 {
			t.Fatalf("current = %d, want 190", tu.current)
		}
	})

	t.Run("returns to base when the heap grows past the floor", func(t *testing.T) {
		tu, applied := newTuner(64*mib, true, 190)
		tu.step()
		if len(*applied) != 1 || (*applied)[0] != 100 {
			t.Fatalf("applied = %v, want [100]", *applied)
		}
	})

	t.Run("no-op when already at the right value", func(t *testing.T) {
		tu, applied := newTuner(11*mib, true, 190)
		tu.step()
		if len(*applied) != 0 {
			t.Fatalf("applied = %v, want no calls", *applied)
		}
	})

	t.Run("no-op on a sub-threshold drift", func(t *testing.T) {
		// 10.9 MiB → GOGC 193, within 10% of the installed 190.
		tu, applied := newTuner(11*mib-100*1024, true, 190)
		tu.step()
		if len(*applied) != 0 {
			t.Fatalf("applied = %v, want no calls (hysteresis)", *applied)
		}
	})

	t.Run("does nothing when the live-heap metric is unavailable", func(t *testing.T) {
		tu, applied := newTuner(0, false, 100)
		tu.step()
		if len(*applied) != 0 {
			t.Fatalf("applied = %v, want no calls", *applied)
		}
	})

	t.Run("does nothing before the first GC", func(t *testing.T) {
		tu, applied := newTuner(0, true, 100)
		tu.step()
		if len(*applied) != 0 {
			t.Fatalf("applied = %v, want no calls", *applied)
		}
	})
}

func TestTunerRunStops(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	tu := &tuner{
		minHeap:  32 << 20,
		base:     100,
		max:      800,
		interval: time.Millisecond,
		current:  100,
		readLive: func() (int64, bool) { return 11 << 20, true },
		setGOGC:  func(int) int { return 100 },
	}
	go func() { tu.run(stop); close(done) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tuner.run did not exit after stop was closed")
	}
}

// ---------------------------------------------------------------------------
// readLiveHeap against the real runtime
// ---------------------------------------------------------------------------

func TestReadLiveHeapIsSupported(t *testing.T) {
	// Guards against a future Go release renaming or dropping the metric,
	// which would silently disable the GOGC floor fleet-wide.
	if _, ok := readLiveHeap(); !ok {
		t.Fatal("/gc/heap/live:bytes is not available on this runtime")
	}
	// After an explicit GC the live heap must be a real, positive figure.
	debug.FreeOSMemory()
	v, ok := readLiveHeap()
	if !ok || v <= 0 {
		t.Fatalf("readLiveHeap after GC = (%d, %v), want a positive value", v, ok)
	}
}

// ---------------------------------------------------------------------------
// killSwitchEnabled
// ---------------------------------------------------------------------------

func TestKillSwitchEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true}, // unset → tuning on
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		{"On", true},
		{"enabled", true},
		{" on ", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"OFF", false},
		{" off ", false},
		{"disabled", false},
		// Unrecognised values fail back to "no tuning", i.e. today's
		// behaviour, rather than to an unintended new one.
		{"maybe", false},
		{"of", false},
		{"2", false},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			if got := killSwitchEnabled(tc.value); got != tc.want {
				t.Fatalf("killSwitchEnabled(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ConfigFromEnv
// ---------------------------------------------------------------------------

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv(EnvKillSwitch, "")
	for _, n := range []string{EnvMemLimitRatio, EnvMinHeapBytes, EnvBaseGOGC, EnvMaxGOGC, EnvInterval} {
		t.Setenv(n, "")
	}
	cfg := ConfigFromEnv()
	want := DefaultConfig()
	if cfg != want {
		t.Fatalf("ConfigFromEnv() = %+v, want %+v", cfg, want)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv(EnvKillSwitch, "on")
	t.Setenv(EnvMemLimitRatio, "0.6")
	t.Setenv(EnvMinHeapBytes, "134217728")
	t.Setenv(EnvBaseGOGC, "75")
	t.Setenv(EnvMaxGOGC, "400")
	t.Setenv(EnvInterval, "5s")

	cfg := ConfigFromEnv()
	if cfg.MemLimitRatio != 0.6 {
		t.Errorf("MemLimitRatio = %g, want 0.6", cfg.MemLimitRatio)
	}
	if cfg.MinHeapTarget != 134217728 {
		t.Errorf("MinHeapTarget = %d, want 134217728", cfg.MinHeapTarget)
	}
	if cfg.BaseGOGC != 75 {
		t.Errorf("BaseGOGC = %d, want 75", cfg.BaseGOGC)
	}
	if cfg.MaxGOGC != 400 {
		t.Errorf("MaxGOGC = %d, want 400", cfg.MaxGOGC)
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %s, want 5s", cfg.Interval)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
}

func TestConfigFromEnvBadValuesFallBackToDefaults(t *testing.T) {
	t.Setenv(EnvKillSwitch, "on")
	t.Setenv(EnvMemLimitRatio, "not-a-float")
	t.Setenv(EnvMinHeapBytes, "-5")
	t.Setenv(EnvBaseGOGC, "zero")
	t.Setenv(EnvMaxGOGC, "0")
	t.Setenv(EnvInterval, "5 parsecs")

	cfg := ConfigFromEnv()
	def := DefaultConfig()
	if cfg.MemLimitRatio != def.MemLimitRatio ||
		cfg.MinHeapTarget != def.MinHeapTarget ||
		cfg.BaseGOGC != def.BaseGOGC ||
		cfg.MaxGOGC != def.MaxGOGC ||
		cfg.Interval != def.Interval {
		t.Fatalf("bad env should degrade to defaults, got %+v", cfg)
	}
}

func TestConfigFromEnvRatioOutOfRange(t *testing.T) {
	for _, v := range []string{"0", "1", "1.5", "-0.2", "100"} {
		t.Run("ratio="+v, func(t *testing.T) {
			t.Setenv(EnvMemLimitRatio, v)
			if got := ConfigFromEnv().MemLimitRatio; got != DefaultMemLimitRatio {
				t.Fatalf("ratio %q accepted as %g, want fallback to %g",
					v, got, DefaultMemLimitRatio)
			}
		})
	}
}

func TestConfigFromEnvMinHeapZeroDisablesFloor(t *testing.T) {
	t.Setenv(EnvMinHeapBytes, "0")
	if got := ConfigFromEnv().MinHeapTarget; got != 0 {
		t.Fatalf("MinHeapTarget = %d, want 0 (floor disabled)", got)
	}
}

func TestConfigFromEnvKillSwitch(t *testing.T) {
	t.Setenv(EnvKillSwitch, "off")
	if ConfigFromEnv().Enabled {
		t.Fatal("FLEET_RUNTIME_TUNING=off must disable tuning")
	}
	t.Setenv(EnvKillSwitch, "on")
	if !ConfigFromEnv().Enabled {
		t.Fatal("FLEET_RUNTIME_TUNING=on must enable tuning")
	}
}

// ---------------------------------------------------------------------------
// normalize
// ---------------------------------------------------------------------------

func TestNormalize(t *testing.T) {
	got := normalize(Config{
		Enabled:       true,
		MemLimitRatio: 3,
		MinHeapTarget: -1,
		BaseGOGC:      -10,
		MaxGOGC:       1,
		Interval:      -time.Second,
	})
	if got.MemLimitRatio != DefaultMemLimitRatio {
		t.Errorf("MemLimitRatio = %g", got.MemLimitRatio)
	}
	if got.MinHeapTarget != 0 {
		t.Errorf("MinHeapTarget = %d, want 0", got.MinHeapTarget)
	}
	if got.BaseGOGC != DefaultBaseGOGC {
		t.Errorf("BaseGOGC = %d", got.BaseGOGC)
	}
	if got.MaxGOGC != DefaultBaseGOGC {
		t.Errorf("MaxGOGC = %d, want it raised to BaseGOGC", got.MaxGOGC)
	}
	if got.Interval != DefaultInterval {
		t.Errorf("Interval = %s", got.Interval)
	}
}

// ---------------------------------------------------------------------------
// applyConfig — end-to-end, asserting the values actually landed
// ---------------------------------------------------------------------------

// testConfig returns a Config pointed at the given fake cgroup files.
func testConfig(v2, v1 string) Config {
	cfg := DefaultConfig()
	cfg.CgroupV2Path = v2
	cfg.CgroupV1Path = v1
	return cfg
}

func TestApplyConfigSetsMemoryLimitFromCgroupV2(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n") // 1 GiB

	res := applyConfig(testConfig(v2, ""), stopper(t))

	if !res.Enabled {
		t.Fatal("Enabled = false")
	}
	if res.CgroupSource != SourceV2 || res.CgroupLimitBytes != 1073741824 {
		t.Fatalf("cgroup detection = (%d, %s)", res.CgroupLimitBytes, res.CgroupSource)
	}
	if res.MemoryLimitSetBy != SetByTuner {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByTuner)
	}
	const want = 858993459 // 0.8 * 1 GiB
	if res.MemoryLimitBytes != want {
		t.Fatalf("MemoryLimitBytes = %d, want %d", res.MemoryLimitBytes, want)
	}
	// The value must actually be installed in the runtime, not just reported.
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("runtime GOMEMLIMIT = %d, want %d", got, want)
	}
}

func TestApplyConfigSetsMemoryLimitFromCgroupV1(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "max\n")
	v1 := writeFile(t, dir, "memory.limit_in_bytes", "536870912\n") // 512 MiB

	res := applyConfig(testConfig(v2, v1), stopper(t))

	if res.CgroupSource != SourceV1 || res.CgroupLimitBytes != 536870912 {
		t.Fatalf("cgroup detection = (%d, %s)", res.CgroupLimitBytes, res.CgroupSource)
	}
	const want = 429496729 // 0.8 * 512 MiB
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("runtime GOMEMLIMIT = %d, want %d", got, want)
	}
}

func TestApplyConfigLeavesMemoryLimitAloneWhenCgroupIsUnlimited(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinel = 7 << 30
	debug.SetMemoryLimit(sentinel)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "max\n")
	v1 := writeFile(t, dir, "memory.limit_in_bytes", "9223372036854771712\n")

	res := applyConfig(testConfig(v2, v1), stopper(t))

	if res.MemoryLimitSetBy != SetByNone {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByNone)
	}
	if got := debug.SetMemoryLimit(-1); got != sentinel {
		t.Fatalf("GOMEMLIMIT was changed to %d, want it left at %d", got, sentinel)
	}
	if !strings.Contains(res.Skipped, "no credible cgroup memory limit") {
		t.Fatalf("Skipped = %q, want it to explain the missing limit", res.Skipped)
	}
}

func TestApplyConfigLeavesMemoryLimitAloneForTinyContainer(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinel = 7 << 30
	debug.SetMemoryLimit(sentinel)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "16777216\n") // 16 MiB

	res := applyConfig(testConfig(v2, ""), stopper(t))

	if res.MemoryLimitSetBy != SetByNone {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByNone)
	}
	if got := debug.SetMemoryLimit(-1); got != sentinel {
		t.Fatalf("GOMEMLIMIT was changed to %d, want it left at %d", got, sentinel)
	}
	if !strings.Contains(res.Skipped, "too small") {
		t.Fatalf("Skipped = %q, want it to mention the too-small limit", res.Skipped)
	}
	// The GOGC floor must still respect the tiny container: half the raw
	// cgroup limit, not the configured 32 MiB default.
	if want := int64(16<<20) / 2; res.MinHeapTarget != want {
		t.Fatalf("MinHeapTarget = %d, want %d (half the cgroup limit)",
			res.MinHeapTarget, want)
	}
}

func TestApplyConfigMissingCgroupFiles(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinel = 5 << 30
	debug.SetMemoryLimit(sentinel)

	dir := t.TempDir()
	res := applyConfig(testConfig(
		filepath.Join(dir, "nope"), filepath.Join(dir, "also-nope")), stopper(t))

	if res.CgroupSource != SourceNone || res.MemoryLimitSetBy != SetByNone {
		t.Fatalf("res = %+v, want no cgroup and no memory limit", res)
	}
	if got := debug.SetMemoryLimit(-1); got != sentinel {
		t.Fatalf("GOMEMLIMIT = %d, want it untouched at %d", got, sentinel)
	}
}

func TestApplyConfigRespectsOperatorGOMEMLIMIT(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	// Stand in for what the runtime would have installed from the env var
	// at startup, then assert we do not move it.
	const operatorLimit = 3 << 30
	debug.SetMemoryLimit(operatorLimit)
	t.Setenv(EnvGOMEMLIMIT, "3GiB")

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n") // would give 0.8 GiB

	res := applyConfig(testConfig(v2, ""), stopper(t))

	if res.MemoryLimitSetBy != SetByOperator {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByOperator)
	}
	if got := debug.SetMemoryLimit(-1); got != operatorLimit {
		t.Fatalf("operator GOMEMLIMIT overridden: got %d, want %d", got, operatorLimit)
	}
	if res.CgroupLimitBytes != 0 {
		t.Errorf("cgroup should not even be consulted, got %d", res.CgroupLimitBytes)
	}
}

func TestApplyConfigRespectsOperatorGOGC(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const operatorGOGC = 37
	debug.SetGCPercent(operatorGOGC)
	t.Setenv(EnvGOGC, "37")

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n")

	res := applyConfig(testConfig(v2, ""), stopper(t))

	if res.GOGCSetBy != SetByOperator {
		t.Fatalf("GOGCSetBy = %s, want %s", res.GOGCSetBy, SetByOperator)
	}
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	if prev != operatorGOGC {
		t.Fatalf("operator GOGC overridden: got %d, want %d", prev, operatorGOGC)
	}
	// Pinning GOGC must not disable GOMEMLIMIT — the knobs are independent.
	if res.MemoryLimitSetBy != SetByTuner {
		t.Fatalf("MemoryLimitSetBy = %s, want GOMEMLIMIT still applied", res.MemoryLimitSetBy)
	}
	if res.MinHeapTarget != 0 {
		t.Errorf("MinHeapTarget = %d, want the floor disabled", res.MinHeapTarget)
	}
}

func TestApplyConfigRespectsOperatorGOGCOff(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	t.Setenv(EnvGOGC, "off")
	debug.SetGCPercent(-1) // GOGC=off

	res := applyConfig(testConfig("", ""), stopper(t))

	if res.GOGCSetBy != SetByOperator {
		t.Fatalf("GOGCSetBy = %s, want %s", res.GOGCSetBy, SetByOperator)
	}
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	if prev != -1 {
		t.Fatalf("GOGC=off was overridden, now %d", prev)
	}
}

func TestApplyConfigTreatsEmptyOperatorEnvAsUnset(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	t.Setenv(EnvGOMEMLIMIT, "")
	t.Setenv(EnvGOGC, "")

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n")

	res := applyConfig(testConfig(v2, ""), stopper(t))

	if res.MemoryLimitSetBy != SetByTuner {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByTuner)
	}
	if res.GOGCSetBy != SetByTuner {
		t.Fatalf("GOGCSetBy = %s, want %s", res.GOGCSetBy, SetByTuner)
	}
}

func TestApplyConfigKillSwitchTouchesNothing(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinelLimit = 6 << 30
	const sentinelGOGC = 42
	debug.SetMemoryLimit(sentinelLimit)
	debug.SetGCPercent(sentinelGOGC)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n")

	cfg := testConfig(v2, "")
	cfg.Enabled = false
	res := applyConfig(cfg, stopper(t))

	if res.Enabled {
		t.Fatal("Enabled = true, want false")
	}
	if res.MemoryLimitSetBy != SetByNone || res.GOGCSetBy != SetByNone {
		t.Fatalf("res = %+v, want nothing set", res)
	}
	if got := debug.SetMemoryLimit(-1); got != sentinelLimit {
		t.Fatalf("GOMEMLIMIT = %d, want %d", got, sentinelLimit)
	}
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	if prev != sentinelGOGC {
		t.Fatalf("GOGC = %d, want %d", prev, sentinelGOGC)
	}
	if res.CgroupLimitBytes != 0 {
		t.Errorf("kill switch must not even read the cgroup, got %d", res.CgroupLimitBytes)
	}
}

// TestApplyConfigKillSwitchViaEnv exercises the documented operator path
// end-to-end: FLEET_RUNTIME_TUNING=off through ConfigFromEnv.
func TestApplyConfigKillSwitchViaEnv(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinelLimit = 6 << 30
	debug.SetMemoryLimit(sentinelLimit)
	t.Setenv(EnvKillSwitch, "off")

	res := applyConfig(ConfigFromEnv(), stopper(t))
	if res.Enabled {
		t.Fatal("FLEET_RUNTIME_TUNING=off did not disable tuning")
	}
	if got := debug.SetMemoryLimit(-1); got != sentinelLimit {
		t.Fatalf("GOMEMLIMIT = %d, want %d", got, sentinelLimit)
	}
}

func TestApplyConfigFloorDisabledByConfig(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	const sentinelGOGC = 42
	debug.SetGCPercent(sentinelGOGC)

	dir := t.TempDir()
	v2 := writeFile(t, dir, "memory.max", "1073741824\n")

	cfg := testConfig(v2, "")
	cfg.MinHeapTarget = 0
	res := applyConfig(cfg, stopper(t))

	if res.GOGCSetBy != SetByNone {
		t.Fatalf("GOGCSetBy = %s, want %s", res.GOGCSetBy, SetByNone)
	}
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	if prev != sentinelGOGC {
		t.Fatalf("GOGC = %d, want it untouched at %d", prev, sentinelGOGC)
	}
	// GOMEMLIMIT still applies.
	if res.MemoryLimitSetBy != SetByTuner {
		t.Fatalf("MemoryLimitSetBy = %s, want %s", res.MemoryLimitSetBy, SetByTuner)
	}
}

func TestApplyConfigClampsFloorToHalfTheMemoryLimit(t *testing.T) {
	guardRuntimeState(t)
	clearRuntimeEnv(t)

	dir := t.TempDir()
	// 64 MiB container → GOMEMLIMIT 53687091 → floor clamped to ~25 MiB.
	v2 := writeFile(t, dir, "memory.max", "67108864\n")

	cfg := testConfig(v2, "")
	cfg.MinHeapTarget = 512 << 20 // absurdly large on purpose
	res := applyConfig(cfg, stopper(t))

	if res.MemoryLimitBytes <= 0 {
		t.Fatalf("expected a memory limit, got %d", res.MemoryLimitBytes)
	}
	if res.MinHeapTarget != res.MemoryLimitBytes/2 {
		t.Fatalf("MinHeapTarget = %d, want %d (half the memory limit)",
			res.MinHeapTarget, res.MemoryLimitBytes/2)
	}
	if res.MinHeapTarget >= res.MemoryLimitBytes {
		t.Fatal("floor must stay below the memory limit")
	}
}

func TestApplyConfigDefaultsAreSane(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Enabled {
		t.Error("tuning should default to enabled")
	}
	if cfg.MemLimitRatio != 0.8 {
		t.Errorf("MemLimitRatio = %g, want 0.8", cfg.MemLimitRatio)
	}
	if cfg.MinHeapTarget != 32<<20 {
		t.Errorf("MinHeapTarget = %d, want 32 MiB", cfg.MinHeapTarget)
	}
	if cfg.BaseGOGC != 100 {
		t.Errorf("BaseGOGC = %d, want 100 (stock Go for normal heaps)", cfg.BaseGOGC)
	}
	if cfg.CgroupV2Path != "/sys/fs/cgroup/memory.max" {
		t.Errorf("CgroupV2Path = %s", cfg.CgroupV2Path)
	}
	if cfg.CgroupV1Path != "/sys/fs/cgroup/memory/memory.limit_in_bytes" {
		t.Errorf("CgroupV1Path = %s", cfg.CgroupV1Path)
	}
}

// ---------------------------------------------------------------------------
// Apply — process-wide entry point
// ---------------------------------------------------------------------------

// TestApplyIsIdempotent relies on TestMain having pinned the kill switch
// off, so the once-guarded global Apply cannot mutate this test binary.
func TestApplyIsIdempotent(t *testing.T) {
	guardRuntimeState(t)

	if _, ok := Applied(); ok {
		t.Fatal("Apply already ran; this test must be the only caller in the package")
	}

	// Apply must emit exactly one startup line describing what it did.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	first := Apply()
	second := Apply()

	logged := logBuf.String()
	if n := strings.Count(logged, "runtimetune:"); n != 1 {
		t.Fatalf("Apply logged %d runtimetune lines, want exactly 1: %q", n, logged)
	}
	if _, ok := Applied(); !ok {
		t.Fatal("Applied() = false after Apply()")
	}
	if first != second {
		t.Fatalf("Apply is not idempotent: %+v vs %+v", first, second)
	}
	if first.Enabled {
		t.Fatal("TestMain pins FLEET_RUNTIME_TUNING=off; Apply should be inert here")
	}
}

// ---------------------------------------------------------------------------
// LogLine
// ---------------------------------------------------------------------------

func TestLogLine(t *testing.T) {
	tests := []struct {
		name     string
		res      Result
		contains []string
	}{
		{
			name: "disabled",
			res:  Result{Enabled: false, Skipped: "FLEET_RUNTIME_TUNING is off"},
			contains: []string{
				"runtimetune: disabled", "FLEET_RUNTIME_TUNING is off",
			},
		},
		{
			name: "fully applied",
			res: Result{
				Enabled:          true,
				CgroupLimitBytes: 1 << 30,
				CgroupSource:     SourceV2,
				MemoryLimitBytes: 858993459,
				MemoryLimitSetBy: SetByTuner,
				GOGCApplied:      190,
				GOGCSetBy:        SetByTuner,
				MinHeapTarget:    32 << 20,
				Interval:         30 * time.Second,
			},
			contains: []string{
				"cgroup_source=cgroupv2", "cgroup_limit=1073741824B",
				"gomemlimit=858993459B", "runtimetune", "gogc=190",
				"min_heap_target=33554432B", "recheck=30s",
			},
		},
		{
			name: "nothing detected",
			res: Result{
				Enabled: true, CgroupSource: SourceNone,
				MemoryLimitSetBy: SetByNone, GOGCSetBy: SetByNone,
				Skipped: "no credible cgroup memory limit",
			},
			contains: []string{
				"cgroup_limit=none", "gomemlimit=none (none)",
				"gogc=untouched", "min_heap_target=off",
				"no credible cgroup memory limit",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line := tc.res.LogLine()
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("LogLine must be one line, got %q", line)
			}
			for _, want := range tc.contains {
				if !strings.Contains(line, want) {
					t.Errorf("LogLine() = %q, missing %q", line, want)
				}
			}
		})
	}
}

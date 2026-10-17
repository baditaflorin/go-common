package chromeguard

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSetOOMScoreAdjRaisesButNeverLowers(t *testing.T) {
	root := t.TempDir()
	in := testInspector(root)
	writeProc(t, root, fakeProc{pid: 10, ppid: 1, comm: "chromium", args: typed("renderer"), oom: 300})
	writeProc(t, root, fakeProc{pid: 11, ppid: 1, comm: "chromium", args: typed("renderer"), oom: 900})
	writeProc(t, root, fakeProc{pid: 12, ppid: 1, comm: "chromium", args: typed("renderer"), oom: -1000})

	if changed, err := in.SetOOMScoreAdj(10, 800); err != nil || !changed || readOOM(t, root, 10) != 800 {
		t.Fatalf("raise 300 -> 800: changed=%v err=%v value=%d", changed, err, readOOM(t, root, 10))
	}
	if changed, err := in.SetOOMScoreAdj(11, 800); err != nil || changed || readOOM(t, root, 11) != 900 {
		t.Fatalf("a score already above adj must stay: changed=%v err=%v value=%d", changed, err, readOOM(t, root, 11))
	}
	if changed, err := in.SetOOMScoreAdj(10, 500); err != nil || changed || readOOM(t, root, 10) != 800 {
		t.Fatalf("lowering must be refused: changed=%v err=%v value=%d", changed, err, readOOM(t, root, 10))
	}
	// A longer old value must not leave digits behind ("-1000" over "800").
	if changed, err := in.SetOOMScoreAdj(12, 800); err != nil || !changed || readOOM(t, root, 12) != 800 {
		t.Fatalf("-1000 -> 800: changed=%v err=%v value=%d", changed, err, readOOM(t, root, 12))
	}
	// Values above the kernel's range are clamped.
	if _, err := in.SetOOMScoreAdj(10, 5000); err != nil || readOOM(t, root, 10) != 1000 {
		t.Fatalf("clamp: err=%v value=%d", err, readOOM(t, root, 10))
	}
	if _, err := in.SetOOMScoreAdj(404, 800); !errors.Is(err, ErrNoSuchProcess) {
		t.Fatalf("missing process = %v", err)
	}
	// It never creates a file in the process directory.
	if err := os.Remove(filepath.Join(root, "11", "oom_score_adj")); err != nil {
		t.Fatal(err)
	}
	if _, err := in.SetOOMScoreAdj(11, 800); !errors.Is(err, ErrNoSuchProcess) {
		t.Fatalf("missing oom_score_adj = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "11", "oom_score_adj")); err == nil {
		t.Fatal("oom_score_adj was created")
	}
}

func TestAdjustRenderersTouchesOnlyRenderersGPUAndUtility(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)

	n, err := in.AdjustRenderers(pidBrowser, 800)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // gpu, utility, two renderers
		t.Fatalf("raised %d scores, want 4", n)
	}
	for _, pid := range []int{pidGPU, pidUtility, pidRendA, pidRendZyg} {
		if got := readOOM(t, root, pid); got != 800 {
			t.Errorf("pid %d oom_score_adj = %d, want 800", pid, got)
		}
	}
	for pid, want := range map[int]int{pidBrowser: 0, pidCrashpad: 0, pidZygote: 300, pidOther: 0, 1: 0, pidRend2: 300, pidBrowser2: 0} {
		if got := readOOM(t, root, pid); got != want {
			t.Errorf("pid %d oom_score_adj = %d, want it untouched at %d", pid, got, want)
		}
	}
}

func TestAdjustRenderersIsIdempotentAndRemembersProcesses(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)
	if n, _ := in.AdjustRenderers(pidBrowser, 800); n != 4 {
		t.Fatalf("first pass raised %d", n)
	}

	// Chromium (or anything) resets one score. The process was handled once, so a
	// second pass leaves it alone instead of fighting.
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pidRendA), "oom_score_adj"), []byte("300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := in.AdjustRenderers(pidBrowser, 800); err != nil || n != 0 || readOOM(t, root, pidRendA) != 300 {
		t.Fatalf("second pass: raised=%d err=%v value=%d", n, err, readOOM(t, root, pidRendA))
	}

	// A new renderer appears: it alone is adjusted.
	writeProc(t, root, fakeProc{pid: 107, ppid: pidBrowser, comm: "chromium", args: typed("renderer"), rssKB: 1, oom: 300})
	if n, _ := in.AdjustRenderers(pidBrowser, 800); n != 1 || readOOM(t, root, 107) != 800 {
		t.Fatalf("new renderer: raised=%d value=%d", n, readOOM(t, root, 107))
	}

	// The pid is reused by a different process (new start time): it is new.
	writeProc(t, root, fakeProc{pid: 107, ppid: pidBrowser, comm: "chromium", args: typed("renderer"), rssKB: 1, oom: 300, start: 424242})
	if n, _ := in.AdjustRenderers(pidBrowser, 800); n != 1 || readOOM(t, root, 107) != 800 {
		t.Fatalf("reused pid: raised=%d value=%d", n, readOOM(t, root, 107))
	}
}

func TestAdjustRenderersDefaultsTo800(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)
	if _, err := in.AdjustRenderers(pidBrowser, 0); err != nil {
		t.Fatal(err)
	}
	if got := readOOM(t, root, pidRendA); got != DefaultOOMScoreAdj || DefaultOOMScoreAdj != 800 {
		t.Fatalf("oom_score_adj = %d, want 800", got)
	}
}

func TestAdjustRenderersReportsAFailedWriteOnce(t *testing.T) {
	root := t.TempDir()
	in := testInspector(root)
	writeProc(t, root, fakeProc{pid: 10, ppid: 1, comm: "chromium", args: browserArgs})
	writeProc(t, root, fakeProc{pid: 11, ppid: 10, comm: "chromium", args: typed("renderer"), oom: 300})
	// A directory where the file should be: the open for writing fails.
	file := filepath.Join(root, "11", "oom_score_adj")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := in.AdjustRenderers(10, 800); err == nil {
		t.Fatal("a failed read of oom_score_adj was swallowed")
	}
	// One attempt per process: the failure is not repeated every pass.
	if _, err := in.AdjustRenderers(10, 800); err != nil {
		t.Fatalf("second pass retried a process already attempted: %v", err)
	}
}

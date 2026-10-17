package chromeguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPage = 4096 // page size the fake statm values are written in

// fakeProc describes one process of a fake /proc tree.
type fakeProc struct {
	pid, ppid int
	comm      string
	args      []string
	rssKB     uint64 // resident set, written as statm pages and status VmRSS
	pssKB     uint64 // 0: no smaps_rollup file
	oom       int
	start     uint64
	noStatm   bool // force the VmRSS fallback
}

// writeProc writes the files the package reads for fp under root.
func writeProc(t *testing.T, root string, fp fakeProc) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(fp.pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if fp.start == 0 {
		fp.start = uint64(1000 + fp.pid)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Fields after "(comm)": state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime priority nice
	// num_threads itrealvalue starttime ...
	write("stat", fmt.Sprintf("%d (%s) S %d %d %d 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 5 0 %d 1000000 %d\n",
		fp.pid, fp.comm, fp.ppid, fp.pid, fp.pid, fp.start, fp.rssKB*1024/testPage))
	if len(fp.args) > 0 {
		write("cmdline", strings.Join(fp.args, "\x00")+"\x00")
	} else {
		write("cmdline", "")
	}
	if !fp.noStatm {
		write("statm", fmt.Sprintf("250000 %d 1000 100 0 5000 0\n", fp.rssKB*1024/testPage))
	}
	write("status", fmt.Sprintf("Name:\t%s\nState:\tS (sleeping)\nVmRSS:\t%8d kB\nRssAnon:\t%8d kB\n", fp.comm, fp.rssKB, fp.rssKB/2))
	write("oom_score_adj", fmt.Sprintf("%d\n", fp.oom))
	if fp.pssKB > 0 {
		write("smaps_rollup", fmt.Sprintf("00400000-ffffffffff601000 ---p 00000000 00:00 0 [rollup]\nRss:%15d kB\nPss:%15d kB\nShared_Clean:%8d kB\n",
			fp.rssKB, fp.pssKB, 100))
	}
}

func removeProc(t *testing.T, root string, pid int) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, strconv.Itoa(pid))); err != nil {
		t.Fatal(err)
	}
}

func readOOM(t *testing.T, root string, pid int) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "oom_score_adj"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("oom_score_adj file = %q", b)
	}
	return n
}

const (
	pidBrowser  = 100
	pidCrashpad = 101
	pidZygote   = 102
	pidGPU      = 103
	pidUtility  = 104
	pidRendZyg  = 105 // renderer forked by the zygote
	pidRendA    = 106
	pidOther    = 200 // not part of the tree
	pidBrowser2 = 300
	pidRend2    = 301
)

var browserArgs = []string{"/usr/bin/chromium", "--headless", "--no-sandbox", "--remote-debugging-port=0"}

func typed(typ string, extra ...string) []string {
	return append([]string{"/usr/bin/chromium", "--type=" + typ}, extra...)
}

// buildTree writes a Chromium-shaped tree plus an unrelated process and a
// second browser, and returns the root.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, fp := range []fakeProc{
		{pid: 1, ppid: 0, comm: "service", args: []string{"/service"}, rssKB: 30_000, oom: 0},
		{pid: pidBrowser, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 120_000, pssKB: 90_000, oom: 0},
		{pid: pidCrashpad, ppid: pidBrowser, comm: "chrome_crashpad", args: []string{"/usr/lib/chromium/chrome_crashpad_handler", "--monitor-self"}, rssKB: 8_000, pssKB: 6_000, oom: 0},
		{pid: pidZygote, ppid: pidBrowser, comm: "chromium", args: typed("zygote", "--no-zygote-sandbox"), rssKB: 40_000, pssKB: 12_000, oom: 300},
		{pid: pidGPU, ppid: pidBrowser, comm: "chromium", args: typed("gpu-process", "--disable-gpu"), rssKB: 60_000, pssKB: 40_000, oom: 300},
		{pid: pidUtility, ppid: pidBrowser, comm: "chromium", args: typed("utility", "--utility-sub-type=network.mojom.NetworkService"), rssKB: 50_000, pssKB: 30_000, oom: 300},
		{pid: pidRendZyg, ppid: pidZygote, comm: "chromium", args: typed("renderer", "--lang=en-US"), rssKB: 200_000, pssKB: 150_000, oom: 300},
		{pid: pidRendA, ppid: pidBrowser, comm: "chromium", args: typed("renderer", "--lang=en-US"), rssKB: 300_000, pssKB: 220_000, oom: 300},
		{pid: pidOther, ppid: 1, comm: "sleep", args: []string{"sleep", "60"}, rssKB: 1_000, oom: 0},
		{pid: pidBrowser2, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 110_000, pssKB: 80_000, oom: 0},
		{pid: pidRend2, ppid: pidBrowser2, comm: "chromium", args: typed("renderer"), rssKB: 70_000, pssKB: 50_000, oom: 300},
	} {
		writeProc(t, root, fp)
	}
	return root
}

func testInspector(root string) *Inspector {
	in := NewInspector(root)
	in.pageSize = testPage
	return in
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

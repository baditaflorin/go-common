package chromeguard

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func pids(procs []Proc) []int {
	out := make([]int, len(procs))
	for i, p := range procs {
		out[i] = p.PID
	}
	return out
}

func TestTreeWalksOnlyTheBrowserSubtree(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)

	procs, err := in.Tree(pidBrowser)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{pidBrowser, pidCrashpad, pidZygote, pidGPU, pidUtility, pidRendA, pidRendZyg} // root, then breadth first by pid
	if got := pids(procs); !reflect.DeepEqual(got, want) {
		t.Fatalf("tree pids = %v, want %v", got, want)
	}

	byPID := map[int]Proc{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	types := map[int]ProcType{
		pidBrowser: TypeBrowser, pidCrashpad: TypeCrashpad, pidZygote: TypeZygote, pidGPU: TypeGPU,
		pidUtility: TypeUtility, pidRendA: TypeRenderer, pidRendZyg: TypeRenderer,
	}
	for pid, typ := range types {
		if byPID[pid].Type != typ {
			t.Errorf("pid %d type = %q, want %q", pid, byPID[pid].Type, typ)
		}
	}
	b := byPID[pidBrowser]
	if b.PPID != 1 || b.Exe != "/usr/bin/chromium" || b.Args[0] != "--headless" || b.RSSBytes != 120_000*1024 ||
		!b.PSSKnown || b.PSSBytes != 90_000*1024 {
		t.Errorf("browser = %+v", b)
	}
	if r := byPID[pidRendA]; r.PPID != pidBrowser || r.Args[0] != "--type=renderer" || r.RSSBytes != 300_000*1024 {
		t.Errorf("renderer = %+v", r)
	}
	if byPID[pidRendZyg].PPID != pidZygote {
		t.Errorf("renderer under the zygote has ppid %d", byPID[pidRendZyg].PPID)
	}

	rss := uint64(120_000+8_000+40_000+60_000+50_000+200_000+300_000) * 1024
	if got := SumRSS(procs); got != rss {
		t.Errorf("SumRSS = %d, want %d", got, rss)
	}
	pss, all := SumPSS(procs)
	if !all || pss != uint64(90_000+6_000+12_000+40_000+30_000+150_000+220_000)*1024 {
		t.Errorf("SumPSS = %d (all=%v)", pss, all)
	}
	// PSS is the smaller, honest number: shared pages are not counted per process.
	if pss >= rss {
		t.Errorf("PSS %d should be below the double-counting RSS %d", pss, rss)
	}
	if got, err := in.TreeRSS(pidBrowser); err != nil || got != rss {
		t.Errorf("TreeRSS = %d, %v; want %d", got, err, rss)
	}
	// The other browser is separate.
	if got, _ := in.TreeRSS(pidBrowser2); got != uint64(110_000+70_000)*1024 {
		t.Errorf("second browser TreeRSS = %d", got)
	}
}

func TestPSSUnknownWhenSmapsRollupMissing(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fakeProc{pid: 10, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 5000, pssKB: 0})
	procs, err := testInspector(root).Tree(10)
	if err != nil || len(procs) != 1 {
		t.Fatalf("Tree = %v, %v", procs, err)
	}
	if procs[0].PSSKnown || procs[0].PSSBytes != 0 || procs[0].RSSBytes != 5000*1024 {
		t.Fatalf("proc = %+v", procs[0])
	}
	if _, all := SumPSS(procs); all {
		t.Fatal("SumPSS reported complete data")
	}
}

func TestRSSFallsBackToStatusVmRSS(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, fakeProc{pid: 10, ppid: 1, comm: "chromium", args: browserArgs, rssKB: 7777, noStatm: true})
	got, err := testInspector(root).TreeRSS(10)
	if err != nil || got != 7777*1024 {
		t.Fatalf("TreeRSS = %d, %v", got, err)
	}
}

func TestStatParsingSurvivesOddCommNames(t *testing.T) {
	cases := map[string]string{
		"plain":              "chromium",
		"with space":         "Web Content",
		"with parens":        "(sd-pam)",
		"with trailing":      "a) S 1 (b",
		"fifteen char limit": "chrome_crashpad",
	}
	for name, comm := range cases {
		t.Run(name, func(t *testing.T) {
			line := "42 (" + comm + ") S 7 42 42 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 5 0 99999 1000000 321\n"
			got, ppid, start, err := parseStat(line)
			if err != nil || got != comm || ppid != 7 || start != 99999 {
				t.Fatalf("parseStat = %q %d %d %v", got, ppid, start, err)
			}
		})
	}
	for _, bad := range []string{"", "garbage", "1 (x) S", "1 (x) S notanumber 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 5 0 9 1 1"} {
		if _, _, _, err := parseStat(bad); err == nil {
			t.Errorf("parseStat(%q) accepted malformed input", bad)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		exe    string
		args   []string
		isRoot bool
		want   ProcType
	}{
		{"/usr/bin/chromium", nil, true, TypeBrowser},
		{"/usr/bin/chromium", []string{"--headless"}, false, TypeBrowser},
		{"/opt/google/chrome/chrome", []string{"--type=renderer", "--lang=en"}, false, TypeRenderer},
		{"chrome", []string{"--type=gpu-process"}, false, TypeGPU},
		{"chrome", []string{"--type=utility", "--utility-sub-type=audio.mojom.AudioService"}, false, TypeUtility},
		{"chrome", []string{"--type=zygote"}, false, TypeZygote},
		{"chrome", []string{"--type=crashpad-handler"}, false, TypeCrashpad},
		{"/usr/lib/chromium/chrome_crashpad_handler", []string{"--monitor-self"}, false, TypeCrashpad},
		{"chrome", []string{"--type=something-new"}, false, TypeOther},
		{"sleep", []string{"60"}, false, TypeOther},
		{"headless_shell", nil, false, TypeBrowser},
		{"sh", []string{"-c", "x"}, true, TypeBrowser}, // the watched root is the browser by definition
	}
	for _, c := range cases {
		if got := classify(c.exe, c.args, c.isRoot); got != c.want {
			t.Errorf("classify(%q, %v, root=%v) = %q, want %q", c.exe, c.args, c.isRoot, got, c.want)
		}
	}
}

func TestTreeErrors(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)
	_, err := in.Tree(99999)
	if !errors.Is(err, ErrNoSuchProcess) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing root = %v, want ErrNoSuchProcess wrapping fs.ErrNotExist", err)
	}
	if _, err := testInspector(filepath.Join(root, "nowhere")).Tree(1); err == nil {
		t.Fatal("unreadable proc root was not reported")
	}
}

func TestTreeSkipsProcessThatExitedMidScan(t *testing.T) {
	root := buildTree(t)
	in := testInspector(root)
	sc, err := in.scan()
	if err != nil {
		t.Fatal(err)
	}
	removeProc(t, root, pidGPU) // gone after the scan, before its details are read
	procs, nerr, err := in.treeFrom(sc, pidBrowser, true)
	if err != nil || nerr != 0 {
		t.Fatalf("treeFrom = %v errs=%d", err, nerr)
	}
	for _, p := range procs {
		if p.PID == pidGPU {
			t.Fatal("exited process listed")
		}
	}
	if len(procs) != 6 {
		t.Fatalf("got %d procs, want 6", len(procs))
	}
}

func TestDefaultRootIsPlatformDependent(t *testing.T) {
	in := NewInspector("")
	if runtime.GOOS == "linux" {
		if in.procRoot != "/proc" {
			t.Fatalf("default root = %q", in.procRoot)
		}
		return
	}
	if _, err := in.Tree(1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Tree on %s = %v, want ErrUnsupported", runtime.GOOS, err)
	}
	if _, err := in.SetOOMScoreAdj(1, 800); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SetOOMScoreAdj on %s = %v, want ErrUnsupported", runtime.GOOS, err)
	}
}

func TestRealProcSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a Linux /proc")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	procs, err := Tree(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var self, child *Proc
	for i := range procs {
		switch procs[i].PID {
		case os.Getpid():
			self = &procs[i]
		case cmd.Process.Pid:
			child = &procs[i]
		}
	}
	if self == nil || self.RSSBytes == 0 || self.Type != TypeBrowser || self.StartTicks == 0 {
		t.Fatalf("self = %+v", self)
	}
	if child == nil || child.PPID != os.Getpid() || filepath.Base(child.Exe) != "sleep" || child.Type != TypeOther {
		t.Fatalf("child = %+v", child)
	}
	if rss, err := TreeRSS(os.Getpid()); err != nil || rss < self.RSSBytes {
		t.Fatalf("TreeRSS = %d, %v", rss, err)
	}
}

func TestParseCmdlineHandlesChromiumsRewrittenArgv(t *testing.T) {
	// Normal: NUL separated.
	if got := parseCmdline([]byte("/usr/bin/chromium\x00--type=renderer\x00--lang=en-US\x00")); !reflect.DeepEqual(got, []string{"/usr/bin/chromium", "--type=renderer", "--lang=en-US"}) {
		t.Fatalf("nul separated = %q", got)
	}
	// Chromium's children rewrite the argument area into one space-joined string
	// (observed on Alpine Chromium 142): it must still classify.
	live := "/usr/lib/chromium/chromium --type=gpu-process --no-sandbox --enable-crash-reporter=,Alpine Linux --user-data-dir=/tmp/p --gpu-preferences=UAAA"
	got := parseCmdline([]byte(live + "\x00"))
	if got[0] != "/usr/lib/chromium/chromium" || classify(got[0], got[1:], false) != TypeGPU {
		t.Fatalf("space joined = %q (type %q)", got, classify(got[0], got[1:], false))
	}
	// An executable path with a space survives.
	got = parseCmdline([]byte("/opt/My Browser/chrome --type=utility --utility-sub-type=network.mojom.NetworkService"))
	if got[0] != "/opt/My Browser/chrome" || classify(got[0], got[1:], false) != TypeUtility {
		t.Fatalf("path with space = %q", got)
	}
	// A lone argument that merely has a space is left alone.
	if got := parseCmdline([]byte("sleep 60\x00")); !reflect.DeepEqual(got, []string{"sleep 60"}) {
		t.Fatalf("plain = %q", got)
	}
	for _, empty := range [][]byte{nil, {}, []byte("\x00"), []byte("\x00\x00")} {
		if got := parseCmdline(empty); len(got) != 0 {
			t.Fatalf("parseCmdline(%q) = %q", empty, got)
		}
	}
}

func TestTreeClassifiesAChromiumTreeWithRewrittenArgv(t *testing.T) {
	root := t.TempDir()
	write := func(pid, ppid int, cmdline string, rssKB uint64) {
		writeProc(t, root, fakeProc{pid: pid, ppid: ppid, comm: "chromium", args: []string{cmdline}, rssKB: rssKB})
	}
	write(11, 1, "/usr/lib/chromium/chromium --headless=new --no-sandbox", 100)
	write(20, 11, "/usr/lib/chromium/chromium --type=zygote --no-zygote-sandbox --no-sandbox", 50)
	write(46, 11, "/usr/lib/chromium/chromium --type=utility --utility-sub-type=network.mojom.NetworkService --lang=en-US", 80)
	write(122, 20, "/usr/lib/chromium/chromium --type=gpu-process --no-sandbox --use-gl=disabled", 50)
	write(82, 20, "/usr/lib/chromium/chromium --type=renderer --crashpad-handler-pid=15 --enable-crash-reporter=,Alpine Linux", 100)
	procs, err := testInspector(root).Tree(11)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]ProcType{11: TypeBrowser, 20: TypeZygote, 46: TypeUtility, 122: TypeGPU, 82: TypeRenderer}
	for _, p := range procs {
		if p.Type != want[p.PID] {
			t.Errorf("pid %d type = %q, want %q (exe %q)", p.PID, p.Type, want[p.PID], p.Exe)
		}
		if p.Exe != "/usr/lib/chromium/chromium" {
			t.Errorf("pid %d exe = %q", p.PID, p.Exe)
		}
	}
}

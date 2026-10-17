package chromeguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ProcType is the role of a process in a Chromium tree, read from its
// --type= argument.
type ProcType string

const (
	TypeBrowser  ProcType = "browser"  // the main process (no --type)
	TypeRenderer ProcType = "renderer" // --type=renderer: one or more tabs
	TypeGPU      ProcType = "gpu"      // --type=gpu-process
	TypeUtility  ProcType = "utility"  // --type=utility (network service, audio, ...)
	TypeZygote   ProcType = "zygote"   // --type=zygote: forks renderers
	TypeCrashpad ProcType = "crashpad" // chrome_crashpad_handler
	TypeOther    ProcType = "other"
)

// DefaultOOMScoreAdj is the oom_score_adj given to renderer-like children when
// the caller does not choose one. Chromium itself starts its renderers at 300
// on Linux; 800 puts them well above the browser (0) and the Go service (0, or
// whatever the container runtime set), so the kernel kills a renderer first.
const DefaultOOMScoreAdj = 800

// ErrUnsupported is returned when there is no proc filesystem to read: the
// default root is /proc on Linux and empty elsewhere. A test (or a caller with
// a mounted copy) can pass a root explicitly on any platform.
var ErrUnsupported = errors.New("chromeguard: no proc filesystem on this platform")

// ErrNoSuchProcess is returned for a root pid that is not in the tree of live
// processes. It wraps fs.ErrNotExist.
var ErrNoSuchProcess = fmt.Errorf("chromeguard: no such process: %w", fs.ErrNotExist)

// Proc is one process of a Chromium tree.
type Proc struct {
	PID  int
	PPID int
	// Exe is the first element of the command line (the comm name when the
	// command line is unreadable); Args are the rest.
	Exe  string
	Args []string
	Type ProcType

	// RSSBytes is the resident set size. Pages shared between processes (the
	// Chromium binary, shared libraries, shared memory) are counted in every
	// process that maps them, so the sum over a tree overstates real use.
	RSSBytes uint64
	// PSSBytes is the proportional set size from smaps_rollup: shared pages
	// are divided among the processes sharing them, so PSS sums to what the
	// tree really holds. Only meaningful when PSSKnown (smaps_rollup needs
	// ptrace access to the process and a kernel with smaps_rollup).
	PSSBytes uint64
	PSSKnown bool

	// StartTicks is the process start time from /proc/<pid>/stat. With PID it
	// identifies a process across pid reuse.
	StartTicks uint64
}

// SumRSS is the sum of RSSBytes (double counting shared pages, see Proc).
func SumRSS(procs []Proc) uint64 {
	var n uint64
	for _, p := range procs {
		n += p.RSSBytes
	}
	return n
}

// SumPSS is the sum of PSSBytes and whether every process reported one.
func SumPSS(procs []Proc) (uint64, bool) {
	var n uint64
	all := len(procs) > 0
	for _, p := range procs {
		n += p.PSSBytes
		all = all && p.PSSKnown
	}
	return n, all
}

// Inspector reads process trees from a proc filesystem. The zero value is not
// usable: call NewInspector. It is safe for concurrent use.
type Inspector struct {
	procRoot string
	pageSize uint64

	mu      sync.Mutex
	touched map[procKey]struct{} // processes whose oom_score_adj was already attempted
}

type procKey struct {
	pid   int
	start uint64
}

// NewInspector returns an Inspector reading from procRoot ("" selects the
// platform default: /proc on Linux, none elsewhere).
func NewInspector(procRoot string) *Inspector {
	if procRoot == "" {
		procRoot = defaultProcRoot()
	}
	return &Inspector{procRoot: procRoot, pageSize: uint64(os.Getpagesize()), touched: make(map[procKey]struct{})}
}

func defaultProcRoot() string {
	if runtime.GOOS == "linux" {
		return "/proc"
	}
	return ""
}

// supported reports whether there is a proc root to read.
func (in *Inspector) supported() bool { return in.procRoot != "" }

var (
	defaultOnce      sync.Once
	defaultInspector *Inspector
)

func theDefault() *Inspector {
	defaultOnce.Do(func() { defaultInspector = NewInspector("") })
	return defaultInspector
}

// Tree returns rootPID and all its descendants using /proc, root first, then
// breadth first by pid. A process that exits during the scan is left out.
func Tree(rootPID int) ([]Proc, error) { return theDefault().Tree(rootPID) }

// TreeRSS returns the summed RSS of rootPID and its descendants using /proc.
// It double counts shared pages: see Proc.RSSBytes and Tree for PSS.
func TreeRSS(rootPID int) (uint64, error) { return theDefault().TreeRSS(rootPID) }

// Tree is the package-level Tree on this Inspector's proc root. PSS is read
// for every process (smaps_rollup is a single small file per process).
func (in *Inspector) Tree(rootPID int) ([]Proc, error) {
	sc, err := in.scan()
	if err != nil {
		return nil, err
	}
	procs, _, err := in.treeFrom(sc, rootPID, true)
	return procs, err
}

// TreeRSS is the package-level TreeRSS on this Inspector's proc root.
func (in *Inspector) TreeRSS(rootPID int) (uint64, error) {
	sc, err := in.scan()
	if err != nil {
		return 0, err
	}
	procs, _, err := in.treeFrom(sc, rootPID, false)
	if err != nil {
		return 0, err
	}
	return SumRSS(procs), nil
}

// snapshot is one pass over the process table: who is whose child.
type snapshot struct {
	ppid     map[int]int
	start    map[int]uint64
	comm     map[int]string
	children map[int][]int
}

// scan reads every /proc/<pid>/stat once. A container's pid namespace is
// small; on a busy host this is the cost to keep in mind.
func (in *Inspector) scan() (*snapshot, error) {
	if !in.supported() {
		return nil, ErrUnsupported
	}
	entries, err := os.ReadDir(in.procRoot)
	if err != nil {
		return nil, fmt.Errorf("chromeguard: read %s: %w", in.procRoot, err)
	}
	sc := &snapshot{
		ppid:     make(map[int]int, len(entries)),
		start:    make(map[int]uint64, len(entries)),
		comm:     make(map[int]string, len(entries)),
		children: make(map[int][]int),
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(path.Join(in.procRoot, e.Name(), "stat"))
		if err != nil {
			continue // exited, or not ours to read
		}
		comm, ppid, start, err := parseStat(string(b))
		if err != nil {
			continue
		}
		sc.ppid[pid], sc.start[pid], sc.comm[pid] = ppid, start, comm
		sc.children[ppid] = append(sc.children[ppid], pid)
	}
	for _, kids := range sc.children {
		sort.Ints(kids)
	}
	return sc, nil
}

// treeFrom walks the snapshot from root and reads each member's details. It
// returns the number of reads that failed for a reason other than the process
// having exited.
func (in *Inspector) treeFrom(sc *snapshot, root int, withPSS bool) (procs []Proc, readErrs int, err error) {
	if _, ok := sc.ppid[root]; !ok {
		return nil, 0, ErrNoSuchProcess
	}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		p, nerr, alive := in.readProc(sc, pid, pid == root, withPSS)
		readErrs += nerr
		if alive {
			procs = append(procs, p)
		}
		for _, kid := range sc.children[pid] {
			if !seen[kid] {
				seen[kid] = true
				queue = append(queue, kid)
			}
		}
	}
	return procs, readErrs, nil
}

// readProc fills in one process. alive is false when the process vanished
// between the scan and the read. nerr counts unexpected read failures.
func (in *Inspector) readProc(sc *snapshot, pid int, isRoot, withPSS bool) (p Proc, nerr int, alive bool) {
	dir := path.Join(in.procRoot, strconv.Itoa(pid))
	p = Proc{PID: pid, PPID: sc.ppid[pid], StartTicks: sc.start[pid]}

	cmd, err := os.ReadFile(path.Join(dir, "cmdline"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, 0, false
	case err != nil:
		nerr++
	}
	args := parseCmdline(cmd)
	if len(args) > 0 {
		p.Exe, p.Args = args[0], args[1:]
	} else {
		p.Exe = sc.comm[pid] // kernel threads, zombies, hidepid
	}
	p.Type = classify(p.Exe, p.Args, isRoot)

	rss, ok, err := in.readRSS(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, nerr, false
	case err != nil:
		nerr++
	}
	if ok {
		p.RSSBytes = rss
	}
	if withPSS {
		if b, err := os.ReadFile(path.Join(dir, "smaps_rollup")); err == nil {
			if kb, ok := parseKB(string(b), "Pss:"); ok {
				p.PSSBytes, p.PSSKnown = kb*1024, true
			}
		}
	}
	return p, nerr, true
}

// readRSS reads the resident set from statm (pages) and falls back to
// VmRSS in status (kB) when statm is missing or unparsable. An error wrapping
// fs.ErrNotExist means the process is gone: both files vanish with it.
func (in *Inspector) readRSS(dir string) (uint64, bool, error) {
	if b, err := os.ReadFile(path.Join(dir, "statm")); err == nil {
		if pages, ok := parseStatm(string(b)); ok {
			return pages * in.pageSize, true, nil
		}
	}
	sb, err := os.ReadFile(path.Join(dir, "status"))
	if err != nil {
		return 0, false, err
	}
	if kb, ok := parseKB(string(sb), "VmRSS:"); ok {
		return kb * 1024, true, nil
	}
	// A zombie has no VmRSS line: it holds no memory.
	return 0, true, nil
}

// parseStat extracts comm, ppid and the start time from /proc/<pid>/stat. comm
// may contain spaces and parentheses, so the fields are counted from the last
// ')'.
func parseStat(s string) (comm string, ppid int, start uint64, err error) {
	open := strings.IndexByte(s, '(')
	closeIdx := strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < open {
		return "", 0, 0, errors.New("malformed stat")
	}
	comm = s[open+1 : closeIdx]
	f := strings.Fields(s[closeIdx+1:])
	// f[0]=state f[1]=ppid ... f[19]=starttime (field 22 of the file).
	if len(f) < 20 {
		return "", 0, 0, errors.New("short stat")
	}
	ppid, err = strconv.Atoi(f[1])
	if err != nil {
		return "", 0, 0, err
	}
	start, err = strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return "", 0, 0, err
	}
	return comm, ppid, start, nil
}

// parseCmdline splits a command line into its arguments. Normally they are
// NUL-separated, but Chromium rewrites the argument area of its child
// processes (renderers, GPU, utility) into one space-joined string, which then
// arrives as a single element: split that on the flags, keeping the executable
// path (which may contain spaces) whole. A flag value with a space in it
// ("--enable-crash-reporter=,Alpine Linux") is split too, which does not matter
// for reading --type.
func parseCmdline(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	args := strings.Split(s, "\x00")
	if len(args) == 1 {
		if i := strings.Index(args[0], " --"); i > 0 {
			return append([]string{args[0][:i]}, strings.Fields(args[0][i:])...)
		}
	}
	return args
}

// parseStatm returns the resident pages (second field of statm).
func parseStatm(s string) (uint64, bool) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0, false
	}
	n, err := strconv.ParseUint(f[1], 10, 64)
	return n, err == nil
}

// parseKB finds a "Key:   1234 kB" line.
func parseKB(s, key string) (uint64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		f := strings.Fields(line[len(key):])
		if len(f) == 0 {
			return 0, false
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		return n, err == nil
	}
	return 0, false
}

// classify reads the role of a process from its --type argument; a process
// without one is the browser (or, by name, the crashpad handler).
func classify(exe string, args []string, isRoot bool) ProcType {
	for _, a := range args {
		v, ok := strings.CutPrefix(a, "--type=")
		if !ok {
			continue
		}
		switch v {
		case "renderer":
			return TypeRenderer
		case "gpu-process":
			return TypeGPU
		case "utility":
			return TypeUtility
		case "zygote":
			return TypeZygote
		case "crashpad-handler":
			return TypeCrashpad
		}
		return TypeOther
	}
	base := path.Base(exe)
	switch {
	case strings.Contains(base, "crashpad"):
		return TypeCrashpad
	case isRoot, strings.Contains(base, "chrom"), strings.Contains(base, "headless"):
		return TypeBrowser
	}
	return TypeOther
}

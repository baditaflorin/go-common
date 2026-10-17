package chromeguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

// SetOOMScoreAdj raises the oom_score_adj of pid to adj and reports whether it
// wrote anything. It never lowers a score: a process already at or above adj is
// left alone. Raising is allowed to the owner without privileges; lowering
// would need CAP_SYS_RESOURCE, which is why this only goes up.
func SetOOMScoreAdj(pid, adj int) (bool, error) { return theDefault().SetOOMScoreAdj(pid, adj) }

// AdjustRenderers raises the oom_score_adj (DefaultOOMScoreAdj when adj is 0)
// of every renderer, GPU and utility process below rootPID that this process
// has not touched yet, so the kernel's OOM killer picks one of those before the
// browser or the Go service. The zygote is left alone: it forks the renderers,
// and the browser does not survive losing it. It is idempotent: a process is
// attempted once (identified by pid and start time, so a reused pid is a new
// process). It returns how many scores were raised.
func AdjustRenderers(rootPID, adj int) (int, error) {
	return theDefault().AdjustRenderers(rootPID, adj)
}

// SetOOMScoreAdj is the package-level SetOOMScoreAdj on this Inspector's proc
// root.
func (in *Inspector) SetOOMScoreAdj(pid, adj int) (bool, error) {
	if !in.supported() {
		return false, ErrUnsupported
	}
	if adj > 1000 {
		adj = 1000
	}
	file := path.Join(in.procRoot, strconv.Itoa(pid), "oom_score_adj")
	b, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, ErrNoSuchProcess
		}
		return false, err
	}
	cur, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return false, fmt.Errorf("chromeguard: parse %s: %w", file, err)
	}
	if cur >= adj {
		return false, nil
	}
	// No O_CREATE: never invent a file. O_TRUNC keeps a shorter value from
	// leaving digits behind in a plain file; procfs ignores it.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, ErrNoSuchProcess
		}
		return false, err
	}
	_, werr := f.WriteString(strconv.Itoa(adj) + "\n")
	cerr := f.Close()
	if werr != nil {
		return false, werr
	}
	return true, cerr
}

// AdjustRenderers is the package-level AdjustRenderers on this Inspector's
// proc root.
func (in *Inspector) AdjustRenderers(rootPID, adj int) (int, error) {
	sc, err := in.scan()
	if err != nil {
		return 0, err
	}
	procs, _, err := in.treeFrom(sc, rootPID, false)
	if err != nil {
		return 0, err
	}
	raised, _, firstErr := in.adjustProcs(procs, adj)
	in.capTouched()
	return raised, firstErr
}

// oomTarget reports whether a process type is one whose score is raised: the
// ones Chromium restarts or a page can do without. Never the browser, the
// zygote or the crash handler.
func oomTarget(t ProcType) bool {
	switch t {
	case TypeRenderer, TypeGPU, TypeUtility:
		return true
	}
	return false
}

// adjustProcs raises the score of the target processes in procs that were not
// attempted before. It returns how many scores were raised, how many writes
// failed (other than because the process exited) and the first such error.
func (in *Inspector) adjustProcs(procs []Proc, adj int) (raised, failed int, firstErr error) {
	if adj == 0 {
		adj = DefaultOOMScoreAdj
	}
	for _, p := range procs {
		if !oomTarget(p.Type) {
			continue
		}
		key := procKey{p.PID, p.StartTicks}
		in.mu.Lock()
		_, done := in.touched[key]
		in.touched[key] = struct{}{} // one attempt per process, success or not
		in.mu.Unlock()
		if done {
			continue
		}
		changed, err := in.SetOOMScoreAdj(p.PID, adj)
		switch {
		case err == nil:
			if changed {
				raised++
			}
		case errors.Is(err, ErrNoSuchProcess):
			// Exited before we got to it.
		default:
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("chromeguard: raise oom_score_adj of pid %d: %w", p.PID, err)
			}
		}
	}
	return raised, failed, firstErr
}

// maxTouched bounds the remembered set when nothing prunes it.
const maxTouched = 8192

// capTouched forgets everything when the remembered set outgrows its bound; a
// forgotten process is simply attempted once more.
func (in *Inspector) capTouched() {
	in.mu.Lock()
	if len(in.touched) > maxTouched {
		in.touched = make(map[procKey]struct{})
	}
	in.mu.Unlock()
}

// pruneTouched drops processes that are gone from sc, so the set follows the
// live tree instead of growing with every renderer ever started.
func (in *Inspector) pruneTouched(sc *snapshot) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for k := range in.touched {
		if start, ok := sc.start[k.pid]; !ok || start != k.start {
			delete(in.touched, k)
		}
	}
}

// Package leakcheck fails a test that leaves goroutines running. It is a small,
// dependency-free stand-in for go.uber.org/goleak.
//
//	func TestServerStopsCleanly(t *testing.T) {
//	    leakcheck.Check(t)   // first, so its cleanup runs last
//	    srv := startServer(t)
//	    ...
//	}
//
// Check snapshots the running goroutines, and when the test (and every cleanup
// registered after it) has finished, waits briefly for stragglers to exit and
// reports any goroutine that is still there, with its stack. Goroutines that
// belong to the runtime, the test framework and net/http's idle-connection loops
// are ignored; use Ignore for anything else that is expected to outlive a test.
//
// It looks at the whole process, so do not use it in a test that calls
// t.Parallel or shares the process with other running tests.
package leakcheck

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// Snapshot is the set of goroutines running when Take was called.
type Snapshot map[int]struct{}

// Option adjusts what counts as a leak.
type Option func(*config)

type config struct{ ignore []string }

// Ignore skips goroutines whose stack contains substr, for example a package's
// background flusher that is started once and never stopped on purpose.
func Ignore(substr string) Option {
	return func(c *config) { c.ignore = append(c.ignore, substr) }
}

// Take records the goroutines that exist now.
func Take() Snapshot {
	s := Snapshot{}
	for id := range stacks() {
		s[id] = struct{}{}
	}
	return s
}

// Leaked returns the stacks of goroutines that are not in before and are not
// ignored. It polls for up to wait, so goroutines that are still winding down
// are not reported; the result is empty once everything has exited.
func Leaked(before Snapshot, wait time.Duration, opts ...Option) []string {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	deadline := time.Now().Add(wait)
	delay := 5 * time.Millisecond
	for {
		extra := leakedNow(before, cfg)
		if len(extra) == 0 || !time.Now().Before(deadline) {
			return extra
		}
		time.Sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
}

// Check snapshots the goroutines now and fails t, at cleanup, if new ones are
// still running.
func Check(t testing.TB, opts ...Option) {
	t.Helper()
	before := Take()
	t.Cleanup(func() {
		if leaked := Leaked(before, 3*time.Second, opts...); len(leaked) > 0 {
			t.Errorf("leakcheck: %d goroutine(s) still running after the test:\n\n%s",
				len(leaked), strings.Join(leaked, "\n\n"))
		}
	})
}

func leakedNow(before Snapshot, cfg config) []string {
	cur := stacks()
	ids := make([]int, 0, len(cur))
	for id := range cur {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var out []string
	for _, id := range ids {
		if _, seen := before[id]; seen {
			continue
		}
		if uninteresting(cur[id], cfg) {
			continue
		}
		out = append(out, cur[id])
	}
	return out
}

// stacks returns every goroutine's stack, keyed by goroutine id.
func stacks() map[int]string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[int]string{}
	for _, rec := range strings.Split(string(buf), "\n\n") {
		rec = strings.TrimSpace(rec)
		var id int
		if _, err := fmt.Sscanf(rec, "goroutine %d [", &id); err != nil {
			continue
		}
		out[id] = rec
	}
	return out
}

// uninteresting reports goroutines that are not the code under test's to clean up.
func uninteresting(stack string, cfg config) bool {
	lines := strings.Split(stack, "\n")
	top := ""
	if len(lines) > 1 {
		top = lines[1]
	}
	for _, prefix := range []string{"runtime.", "testing.", "os/signal."} {
		if strings.HasPrefix(top, prefix) {
			return true
		}
	}
	// A transport's idle keep-alive loops end on their own timer, not when a
	// test finishes.
	if strings.Contains(stack, "net/http.(*persistConn).readLoop") ||
		strings.Contains(stack, "net/http.(*persistConn).writeLoop") {
		return true
	}
	for _, s := range cfg.ignore {
		if strings.Contains(stack, s) {
			return true
		}
	}
	return false
}

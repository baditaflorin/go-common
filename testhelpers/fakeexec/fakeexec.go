// Package fakeexec provides an in-memory fake for the command-runner seam
// used across the fleet to shell out to git / go / docker:
//
//	run(dir, name string, args ...string) (string, error)
//
// Production code keeps a swappable package var (e.g. `var sh = run`) and calls
// it instead of os/exec directly. Tests swap in Fake.Run to assert
// *orchestration* — which commands run, in what order, and how the code reacts
// to their exit status — without invoking the real toolchain or touching the
// network. Side-effect hooks let a faked `go mod vendor` simulate writing files
// so downstream logic (change detection, staging) still sees them.
//
// It exists so every fleet app can self-test its own shell-outs the same way,
// with no third-party dependency: this package imports only the standard
// library.
//
// Usage:
//
//	f := fakeexec.New().
//	    OnDo("go mod vendor", func(c fakeexec.Call) error {
//	        return os.WriteFile(filepath.Join(c.Dir, "vendor/modules.txt"), data, 0o644)
//	    }).
//	    FailOn("go build", errors.New("undefined: Foo")).
//	    OnReturn("git status --porcelain", " M go.mod\n", nil)
//
//	old := sh
//	sh = f.Run
//	t.Cleanup(func() { sh = old })
//	// ... exercise the code under test ...
//	f.AssertOrder(t, "go mod vendor", "go build")
//	f.AssertCalled(t, "git add -A")
//
// A Fake is safe for concurrent use.
package fakeexec

import (
	"strings"
	"sync"
	"testing"
)

// Call is one recorded invocation of the runner seam.
type Call struct {
	Dir  string
	Name string
	Args []string
}

// Line renders the call as a shell-ish string: "name arg1 arg2". Rule matching
// and the Assert helpers all work against this rendering, so a match string
// like "git reset --hard" or "go mod vendor" reads naturally.
func (c Call) Line() string {
	if len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + strings.Join(c.Args, " ")
}

type rule struct {
	match string
	out   string
	err   error
	hook  func(Call) error
}

// Fake is an in-memory runner. Configure outcomes with the On* / FailOn
// methods, then pass Fake.Run wherever a run(dir, name, args...) (string,
// error) seam is expected. Commands no rule matches succeed with empty output
// by default; override that with SetDefault.
type Fake struct {
	mu     sync.Mutex
	rules  []rule
	calls  []Call
	defOut string
	defErr error
}

// New returns a Fake whose unmatched commands succeed with empty output.
func New() *Fake { return &Fake{} }

// SetDefault sets the (out, err) returned for commands that match no rule.
// Useful to make an unconfigured command fail loudly instead of silently
// succeeding.
func (f *Fake) SetDefault(out string, err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defOut, f.defErr = out, err
	return f
}

// OnReturn registers a rule: a command whose rendered line contains match
// returns (out, err). Rules are tried in registration order; the first match
// wins.
func (f *Fake) OnReturn(match, out string, err error) *Fake {
	return f.add(rule{match: match, out: out, err: err})
}

// FailOn registers a rule that makes commands containing match return
// ("", err) — i.e. a non-zero exit.
func (f *Fake) FailOn(match string, err error) *Fake {
	return f.add(rule{match: match, err: err})
}

// OnDo registers a rule that runs hook (a simulated side effect, e.g. writing
// a file a real command would have written) and returns ("", hook's error).
func (f *Fake) OnDo(match string, hook func(Call) error) *Fake {
	return f.add(rule{match: match, hook: hook})
}

func (f *Fake) add(r rule) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, r)
	return f
}

// Run is the seam. Assign it where a run(dir, name, args...) (string, error)
// function value is expected.
func (f *Fake) Run(dir, name string, args ...string) (string, error) {
	c := Call{Dir: dir, Name: name, Args: append([]string(nil), args...)}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	rules := append([]rule(nil), f.rules...)
	defOut, defErr := f.defOut, f.defErr
	f.mu.Unlock()

	line := c.Line()
	for _, r := range rules {
		if !strings.Contains(line, r.match) {
			continue
		}
		if r.hook != nil {
			if err := r.hook(c); err != nil {
				return "", err
			}
		}
		return r.out, r.err
	}
	return defOut, defErr
}

// Calls returns a snapshot of every recorded invocation, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Lines returns the recorded calls rendered via Call.Line, in order.
func (f *Fake) Lines() []string {
	calls := f.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Line()
	}
	return out
}

// Count returns how many recorded calls contain match.
func (f *Fake) Count(match string) int {
	n := 0
	for _, l := range f.Lines() {
		if strings.Contains(l, match) {
			n++
		}
	}
	return n
}

// Reset clears recorded calls (rules are kept).
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// AssertCalled fails t if no recorded call contains match.
func (f *Fake) AssertCalled(t testing.TB, match string) {
	t.Helper()
	if f.Count(match) == 0 {
		t.Errorf("fakeexec: expected a command containing %q, got:\n  %s",
			match, strings.Join(f.Lines(), "\n  "))
	}
}

// AssertNotCalled fails t if any recorded call contains match.
func (f *Fake) AssertNotCalled(t testing.TB, match string) {
	t.Helper()
	if n := f.Count(match); n != 0 {
		t.Errorf("fakeexec: expected NO command containing %q, got %d:\n  %s",
			match, n, strings.Join(f.Lines(), "\n  "))
	}
}

// AssertOrder fails t unless a command containing each match appears, in the
// given relative order (matched against first occurrences). Intervening
// commands are allowed — it asserts ordering, not adjacency.
func (f *Fake) AssertOrder(t testing.TB, matches ...string) {
	t.Helper()
	lines := f.Lines()
	from := 0
	for _, m := range matches {
		found := -1
		for i := from; i < len(lines); i++ {
			if strings.Contains(lines[i], m) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Errorf("fakeexec: %q not found in expected order; calls were:\n  %s",
				m, strings.Join(lines, "\n  "))
			return
		}
		from = found + 1
	}
}

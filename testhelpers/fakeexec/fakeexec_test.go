package fakeexec

import (
	"errors"
	"fmt"
	"testing"
)

// spyTB implements just enough of testing.TB to capture Errorf/Helper so the
// Assert* helpers can be tested without failing the real test. Embedding the
// testing.TB interface (nil value) satisfies the rest; only Helper and Errorf
// are ever called by the helpers under test.
type spyTB struct {
	testing.TB
	failed bool
	msgs   []string
}

func (s *spyTB) Helper() {}
func (s *spyTB) Errorf(format string, args ...any) {
	s.failed = true
	s.msgs = append(s.msgs, fmt.Sprintf(format, args...))
}

func TestCallLine(t *testing.T) {
	if got := (Call{Name: "go"}).Line(); got != "go" {
		t.Errorf("Line() no-args = %q, want %q", got, "go")
	}
	got := (Call{Name: "git", Args: []string{"reset", "--hard", "origin/main"}}).Line()
	if want := "git reset --hard origin/main"; got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
}

func TestRun_RecordsAndDefaults(t *testing.T) {
	f := New()
	out, err := f.Run("/repo", "go", "mod", "tidy")
	if out != "" || err != nil {
		t.Errorf("unconfigured Run = (%q, %v), want empty success", out, err)
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(calls))
	}
	if calls[0].Dir != "/repo" || calls[0].Line() != "go mod tidy" {
		t.Errorf("recorded call = %+v", calls[0])
	}
}

func TestSetDefault_FailsUnmatched(t *testing.T) {
	sentinel := errors.New("boom")
	f := New().SetDefault("", sentinel)
	if _, err := f.Run("/x", "git", "status"); !errors.Is(err, sentinel) {
		t.Errorf("default err = %v, want %v", err, sentinel)
	}
}

func TestOnReturn_FirstMatchWins(t *testing.T) {
	f := New().
		OnReturn("git status --porcelain", " M go.mod\n", nil).
		OnReturn("git status", "should-not-reach", nil) // shadowed by the first
	out, err := f.Run("/r", "git", "status", "--porcelain")
	if err != nil || out != " M go.mod\n" {
		t.Errorf("Run = (%q, %v), want (%q, nil)", out, err, " M go.mod\n")
	}
}

func TestFailOn(t *testing.T) {
	sentinel := errors.New("undefined: Foo")
	f := New().FailOn("go build", sentinel)
	if _, err := f.Run("/r", "go", "build", "./..."); !errors.Is(err, sentinel) {
		t.Errorf("FailOn err = %v, want %v", err, sentinel)
	}
	// A non-matching command still succeeds by default.
	if _, err := f.Run("/r", "go", "vet", "./..."); err != nil {
		t.Errorf("unmatched command should succeed, got %v", err)
	}
}

func TestOnDo_RunsHookWithSideEffect(t *testing.T) {
	var gotDir string
	f := New().OnDo("go mod vendor", func(c Call) error {
		gotDir = c.Dir
		return nil
	})
	if _, err := f.Run("/repo/x", "go", "mod", "vendor"); err != nil {
		t.Fatalf("OnDo Run err = %v", err)
	}
	if gotDir != "/repo/x" {
		t.Errorf("hook saw dir %q, want %q", gotDir, "/repo/x")
	}
}

func TestOnDo_HookErrorPropagates(t *testing.T) {
	sentinel := errors.New("disk full")
	f := New().OnDo("go mod vendor", func(Call) error { return sentinel })
	if _, err := f.Run("/r", "go", "mod", "vendor"); !errors.Is(err, sentinel) {
		t.Errorf("hook error = %v, want %v", err, sentinel)
	}
}

func TestCountAndReset(t *testing.T) {
	f := New()
	f.Run("/r", "git", "add", "-A")
	f.Run("/r", "git", "commit", "-m", "x")
	f.Run("/r", "git", "add", "go.mod")
	if n := f.Count("git add"); n != 2 {
		t.Errorf("Count(git add) = %d, want 2", n)
	}
	f.Reset()
	if n := len(f.Calls()); n != 0 {
		t.Errorf("after Reset, Calls len = %d, want 0", n)
	}
}

func TestAssertCalled(t *testing.T) {
	f := New()
	f.Run("/r", "git", "add", "-A")

	ok := &spyTB{}
	f.AssertCalled(ok, "git add -A")
	if ok.failed {
		t.Errorf("AssertCalled should pass; msgs=%v", ok.msgs)
	}

	bad := &spyTB{}
	f.AssertCalled(bad, "go mod vendor")
	if !bad.failed {
		t.Error("AssertCalled should fail for an uncalled command")
	}
}

func TestAssertNotCalled(t *testing.T) {
	f := New()
	f.Run("/r", "git", "add", "go.mod", "go.sum")

	ok := &spyTB{}
	f.AssertNotCalled(ok, "git add -A")
	if ok.failed {
		t.Errorf("AssertNotCalled should pass; msgs=%v", ok.msgs)
	}

	bad := &spyTB{}
	f.AssertNotCalled(bad, "git add")
	if !bad.failed {
		t.Error("AssertNotCalled should fail when the command was called")
	}
}

func TestAssertOrder(t *testing.T) {
	f := New()
	f.Run("/r", "go", "get", "mod@v1")
	f.Run("/r", "go", "mod", "tidy")
	f.Run("/r", "go", "mod", "vendor")
	f.Run("/r", "go", "build", "./...")

	ok := &spyTB{}
	f.AssertOrder(ok, "go mod vendor", "go build")
	if ok.failed {
		t.Errorf("AssertOrder should pass for correct order; msgs=%v", ok.msgs)
	}

	bad := &spyTB{}
	f.AssertOrder(bad, "go build", "go mod vendor") // wrong order
	if !bad.failed {
		t.Error("AssertOrder should fail when commands are out of order")
	}
}

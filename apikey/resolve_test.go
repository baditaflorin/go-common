package apikey

import "testing"

func TestResolve_FirstNonEmptyWins(t *testing.T) {
	t.Setenv("A", "")
	t.Setenv("B", "from-b")
	t.Setenv("C", "from-c")
	r := Resolve("A", "B", "C")
	if !r.Found || r.Key != "from-b" || r.Source != "B" {
		t.Fatalf("got %+v, want Found=true Key=from-b Source=B", r)
	}
}

func TestResolve_AllUnsetReturnsNotFound(t *testing.T) {
	t.Setenv("X1_NOT_SET", "")
	t.Setenv("X2_NOT_SET", "")
	r := Resolve("X1_NOT_SET", "X2_NOT_SET")
	if r.Found || r.Key != "" || r.Source != "" {
		t.Fatalf("got %+v, want zero ResolveResult", r)
	}
}

func TestResolve_EmptyVarListReturnsNotFound(t *testing.T) {
	r := Resolve()
	if r.Found || r.Key != "" || r.Source != "" {
		t.Fatalf("got %+v, want zero ResolveResult", r)
	}
}

func TestResolve_EmptyValueSkipped(t *testing.T) {
	// An env var set to "" should be treated as unset so callers can
	// blank-disable a variable without re-ordering the chain.
	t.Setenv("FIRST", "")
	t.Setenv("SECOND", "wins")
	r := Resolve("FIRST", "SECOND")
	if r.Source != "SECOND" || r.Key != "wins" {
		t.Fatalf("got %+v, want SECOND=wins", r)
	}
}

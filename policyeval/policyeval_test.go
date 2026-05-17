package policyeval_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/policyeval"
)

// decision is the typical caller-defined Decision shape — Channel +
// Priority + Tags. Used throughout the tests to confirm the engine
// doesn't peek inside whatever struct callers pass.
type decision struct {
	Channel  string
	Priority int
	Tags     []string
}

func TestEvaluate_EmptyRules(t *testing.T) {
	res, err := policyeval.Evaluate(nil, policyeval.Fact{"severity": "high"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Decision != nil {
		t.Errorf("expected nil Decision, got %#v", res.Decision)
	}
	if len(res.Matched) != 0 {
		t.Errorf("expected no Matched, got %v", res.Matched)
	}
	if res.Explanation != "" {
		t.Errorf("expected empty Explanation, got %q", res.Explanation)
	}
}

func TestEvaluate_AllOps(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	later := now.Add(time.Hour)

	// One sub-test per operator with a passing AND a failing case so a
	// regression in either direction surfaces individually rather than
	// silently flipping behaviour.
	cases := []struct {
		name string
		pred policyeval.Predicate
		fact policyeval.Fact
		want bool
	}{
		{"eq pass", policyeval.Predicate{Field: "env", Op: policyeval.Eq, Value: "prod"}, policyeval.Fact{"env": "prod"}, true},
		{"eq fail", policyeval.Predicate{Field: "env", Op: policyeval.Eq, Value: "prod"}, policyeval.Fact{"env": "stage"}, false},
		{"ne pass", policyeval.Predicate{Field: "env", Op: policyeval.Ne, Value: "prod"}, policyeval.Fact{"env": "stage"}, true},
		{"ne fail", policyeval.Predicate{Field: "env", Op: policyeval.Ne, Value: "prod"}, policyeval.Fact{"env": "prod"}, false},
		{"lt pass int", policyeval.Predicate{Field: "n", Op: policyeval.Lt, Value: 10}, policyeval.Fact{"n": 5}, true},
		{"lt fail int", policyeval.Predicate{Field: "n", Op: policyeval.Lt, Value: 10}, policyeval.Fact{"n": 20}, false},
		{"le pass float vs int", policyeval.Predicate{Field: "n", Op: policyeval.Le, Value: 10}, policyeval.Fact{"n": 10.0}, true},
		{"gt pass time", policyeval.Predicate{Field: "ts", Op: policyeval.Gt, Value: earlier}, policyeval.Fact{"ts": now}, true},
		{"ge fail time", policyeval.Predicate{Field: "ts", Op: policyeval.Ge, Value: later}, policyeval.Fact{"ts": now}, false},
		{"in pass", policyeval.Predicate{Field: "sev", Op: policyeval.In, Value: []any{"critical", "high"}}, policyeval.Fact{"sev": "high"}, true},
		{"in fail", policyeval.Predicate{Field: "sev", Op: policyeval.In, Value: []any{"critical", "high"}}, policyeval.Fact{"sev": "low"}, false},
		{"not_in pass", policyeval.Predicate{Field: "sev", Op: policyeval.NotIn, Value: []any{"critical", "high"}}, policyeval.Fact{"sev": "low"}, true},
		{"not_in fail", policyeval.Predicate{Field: "sev", Op: policyeval.NotIn, Value: []any{"critical", "high"}}, policyeval.Fact{"sev": "high"}, false},
		{"contains substr pass", policyeval.Predicate{Field: "msg", Op: policyeval.Contains, Value: "leak"}, policyeval.Fact{"msg": "secret leak detected"}, true},
		{"contains substr fail", policyeval.Predicate{Field: "msg", Op: policyeval.Contains, Value: "leak"}, policyeval.Fact{"msg": "clean"}, false},
		{"contains slice pass", policyeval.Predicate{Field: "tags", Op: policyeval.Contains, Value: "prod"}, policyeval.Fact{"tags": []string{"prod", "eu"}}, true},
		{"contains slice fail", policyeval.Predicate{Field: "tags", Op: policyeval.Contains, Value: "prod"}, policyeval.Fact{"tags": []string{"dev"}}, false},
		{"matches pass", policyeval.Predicate{Field: "host", Op: policyeval.Matches, Value: `^api\..*\.com$`}, policyeval.Fact{"host": "api.example.com"}, true},
		{"matches fail", policyeval.Predicate{Field: "host", Op: policyeval.Matches, Value: `^api\.`}, policyeval.Fact{"host": "web.example.com"}, false},
		{"exists pass", policyeval.Predicate{Field: "env", Op: policyeval.Exists}, policyeval.Fact{"env": "prod"}, true},
		{"exists fail nil", policyeval.Predicate{Field: "env", Op: policyeval.Exists}, policyeval.Fact{"env": nil}, false},
		{"exists fail missing", policyeval.Predicate{Field: "env", Op: policyeval.Exists}, policyeval.Fact{}, false},
		{"not_exists pass", policyeval.Predicate{Field: "env", Op: policyeval.NotExists}, policyeval.Fact{}, true},
		{"not_exists fail", policyeval.Predicate{Field: "env", Op: policyeval.NotExists}, policyeval.Fact{"env": "prod"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := []policyeval.Rule{
				{Name: "t", When: []policyeval.Predicate{tc.pred}, Then: "hit"},
			}
			res, err := policyeval.Evaluate(rules, tc.fact)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := res.Decision == "hit"
			if got != tc.want {
				t.Errorf("op %s: got match=%v, want %v (matched=%v)", tc.pred.Op, got, tc.want, res.Matched)
			}
		})
	}
}

func TestEvaluate_DottedField_Map(t *testing.T) {
	fact := policyeval.Fact{
		"target": map[string]any{
			"severity": "critical",
			"nested":   map[string]any{"score": 42},
		},
	}
	rules := []policyeval.Rule{{
		Name: "deep",
		When: []policyeval.Predicate{
			{Field: "target.severity", Op: policyeval.Eq, Value: "critical"},
			{Field: "target.nested.score", Op: policyeval.Ge, Value: 40},
		},
		Then: "hit",
	}}
	res, err := policyeval.Evaluate(rules, fact)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Decision != "hit" {
		t.Fatalf("expected hit, got %#v (matched=%v)", res.Decision, res.Matched)
	}
}

func TestEvaluate_DottedField_Struct(t *testing.T) {
	type Inner struct{ Severity string }
	type Outer struct{ Target Inner }
	fact := policyeval.Fact{"req": Outer{Target: Inner{Severity: "high"}}}
	rules := []policyeval.Rule{{
		Name: "struct-walk",
		When: []policyeval.Predicate{
			{Field: "req.Target.Severity", Op: policyeval.Eq, Value: "high"},
		},
		Then: "hit",
	}}
	res, err := policyeval.Evaluate(rules, fact)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Decision != "hit" {
		t.Fatalf("expected struct hit, got %#v", res.Decision)
	}
}

func TestEvaluate_DottedField_MissingIntermediate(t *testing.T) {
	// `target.severity` where target is missing: predicate is false
	// for Eq (not an error), and true for NotExists.
	fact := policyeval.Fact{"other": 1}
	resEq, err := policyeval.Evaluate([]policyeval.Rule{{
		Name: "eq",
		When: []policyeval.Predicate{{Field: "target.severity", Op: policyeval.Eq, Value: "x"}},
		Then: "hit",
	}}, fact)
	if err != nil || resEq.Decision != nil {
		t.Fatalf("missing path Eq: err=%v decision=%v", err, resEq.Decision)
	}
	resNX, err := policyeval.Evaluate([]policyeval.Rule{{
		Name: "nx",
		When: []policyeval.Predicate{{Field: "target.severity", Op: policyeval.NotExists}},
		Then: "hit",
	}}, fact)
	if err != nil || resNX.Decision != "hit" {
		t.Fatalf("missing path NotExists: err=%v decision=%v", err, resNX.Decision)
	}
}

func TestEvaluate_ChainedRules_LastNonStopWins(t *testing.T) {
	rules := []policyeval.Rule{
		{
			Name: "default-low",
			Then: decision{Channel: "log", Priority: 1},
		},
		{
			Name: "elevate-on-prod",
			When: []policyeval.Predicate{{Field: "env", Op: policyeval.Eq, Value: "prod"}},
			Then: decision{Channel: "slack-incident", Priority: 5},
		},
	}
	res, err := policyeval.Evaluate(rules, policyeval.Fact{"env": "prod"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	d, ok := res.Decision.(decision)
	if !ok || d.Channel != "slack-incident" {
		t.Fatalf("expected elevate-on-prod to win, got %#v (matched=%v)", res.Decision, res.Matched)
	}
	if len(res.Matched) != 2 {
		t.Errorf("expected both rules in matched trail, got %v", res.Matched)
	}
}

func TestEvaluate_StopRule(t *testing.T) {
	rules := []policyeval.Rule{
		{
			Name: "explicit-deny",
			When: []policyeval.Predicate{{Field: "blocked", Op: policyeval.Eq, Value: true}},
			Then: decision{Channel: "drop"},
			Stop: true,
		},
		{
			Name: "default-allow",
			Then: decision{Channel: "queue"},
		},
	}
	res, err := policyeval.Evaluate(rules, policyeval.Fact{"blocked": true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	d, ok := res.Decision.(decision)
	if !ok || d.Channel != "drop" {
		t.Fatalf("Stop rule should short-circuit; got %#v (matched=%v)", res.Decision, res.Matched)
	}
	if len(res.Matched) != 1 {
		t.Errorf("expected only the stop rule in trail, got %v", res.Matched)
	}
}

func TestEvaluate_Explanation(t *testing.T) {
	rules := []policyeval.Rule{{
		Name: "high-severity-prod",
		When: []policyeval.Predicate{
			{Field: "severity", Op: policyeval.In, Value: []any{"critical", "high"}},
			{Field: "env", Op: policyeval.Eq, Value: "prod"},
		},
		Then: "page",
	}}
	res, err := policyeval.Evaluate(rules, policyeval.Fact{"severity": "high", "env": "prod"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// The exact format is intentionally not contract-locked, but each
	// listed substring must appear so log-readers can follow it.
	want := []string{"high-severity-prod", "severity in", "env eq prod", "AND"}
	for _, w := range want {
		if !strings.Contains(res.Explanation, w) {
			t.Errorf("explanation missing %q: %s", w, res.Explanation)
		}
	}
}

func TestEvaluate_TypeMismatch(t *testing.T) {
	// "Compare string to int via Lt returns a clear error."
	rules := []policyeval.Rule{{
		Name: "bad-cmp",
		When: []policyeval.Predicate{{Field: "n", Op: policyeval.Lt, Value: 10}},
		Then: "x",
	}}
	_, err := policyeval.Evaluate(rules, policyeval.Fact{"n": "five"})
	if err == nil {
		t.Fatal("expected error from string-vs-int Lt, got nil")
	}
	if !strings.Contains(err.Error(), "lt") {
		t.Errorf("error should name the op, got: %v", err)
	}
}

func TestEvaluate_BadRegex(t *testing.T) {
	rules := []policyeval.Rule{{
		Name: "bad-re",
		When: []policyeval.Predicate{{Field: "host", Op: policyeval.Matches, Value: `[unterminated`}},
		Then: "x",
	}}
	_, err := policyeval.Evaluate(rules, policyeval.Fact{"host": "x"})
	if err == nil {
		t.Fatal("expected bad-regex error")
	}
}

func TestEvaluate_UnknownOp(t *testing.T) {
	rules := []policyeval.Rule{{
		Name: "bogus",
		When: []policyeval.Predicate{{Field: "x", Op: policyeval.Op("xyz"), Value: 1}},
		Then: "x",
	}}
	_, err := policyeval.Evaluate(rules, policyeval.Fact{"x": 1})
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
	if !errors.Is(err, policyeval.ErrUnknownOp) {
		t.Errorf("expected ErrUnknownOp, got %v", err)
	}
}

func TestEvaluate_Concurrent(t *testing.T) {
	// 100 goroutines pummel the same ruleset to surface data races
	// under `go test -race`. The ruleset itself is read-only; only
	// the regex cache writes (once on first call).
	//
	// Default first, specific override after — last non-Stop match
	// wins, so the high-prod rule should always be the visible
	// decision.
	rules := []policyeval.Rule{
		{
			Name: "default",
			Then: decision{Channel: "log", Priority: 1},
		},
		{
			Name: "high-prod",
			When: []policyeval.Predicate{
				{Field: "severity", Op: policyeval.In, Value: []any{"critical", "high"}},
				{Field: "env", Op: policyeval.Eq, Value: "prod"},
				{Field: "host", Op: policyeval.Matches, Value: `^api\.`},
			},
			Then: decision{Channel: "page", Priority: 9},
		},
	}
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			fact := policyeval.Fact{
				"severity": "high",
				"env":      "prod",
				"host":     "api.example.com",
			}
			res, err := policyeval.Evaluate(rules, fact)
			if err != nil {
				t.Errorf("goroutine %d: err=%v", i, err)
				return
			}
			if d, ok := res.Decision.(decision); !ok || d.Channel != "page" {
				t.Errorf("goroutine %d: wrong decision %#v", i, res.Decision)
			}
		}(i)
	}
	wg.Wait()
}

func BenchmarkEvaluate_1000Rules(b *testing.B) {
	rules := make([]policyeval.Rule, 0, 1000)
	for i := 0; i < 999; i++ {
		// 999 cheap never-match rules so we measure traversal cost,
		// not a single match. Catches accidental O(rules²) shapes
		// (e.g. allocating per-rule slices proportional to position).
		rules = append(rules, policyeval.Rule{
			Name: fmt.Sprintf("noop-%d", i),
			When: []policyeval.Predicate{{Field: "marker", Op: policyeval.Eq, Value: i}},
			Then: i,
		})
	}
	rules = append(rules, policyeval.Rule{
		Name: "final",
		When: []policyeval.Predicate{{Field: "env", Op: policyeval.Eq, Value: "prod"}},
		Then: "hit",
	})
	fact := policyeval.Fact{"marker": -1, "env": "prod"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := policyeval.Evaluate(rules, fact)
		if err != nil {
			b.Fatal(err)
		}
	}
}

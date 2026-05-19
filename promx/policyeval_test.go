package promx

import (
	"errors"
	"testing"

	"github.com/baditaflorin/go-common/policyeval"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPolicyCollectors_ObservePolicy(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPolicyCollectors(reg)

	// Matched: 2 rules fired, winner is the last.
	c.ObservePolicy(policyeval.Event{Ruleset: "leak-bounty", Matched: []string{"high-sev", "to-slack"}, Winner: "to-slack"})
	// No-match.
	c.ObservePolicy(policyeval.Event{Ruleset: "leak-bounty"})
	// Error.
	c.ObservePolicy(policyeval.Event{Ruleset: "target-rep", Err: errors.New("bad regex")})

	if v := testutil.ToFloat64(c.evaluations.WithLabelValues(c.service, "leak-bounty", "matched")); v != 1 {
		t.Fatalf("eval(matched) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.evaluations.WithLabelValues(c.service, "leak-bounty", "no_match")); v != 1 {
		t.Fatalf("eval(no_match) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.errors.WithLabelValues(c.service, "target-rep")); v != 1 {
		t.Fatalf("errors(target-rep) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.fires.WithLabelValues(c.service, "leak-bounty", "high-sev")); v != 1 {
		t.Fatalf("fires(high-sev) = %v, want 1", v)
	}
	if v := testutil.ToFloat64(c.fires.WithLabelValues(c.service, "leak-bounty", "to-slack")); v != 1 {
		t.Fatalf("fires(to-slack) = %v, want 1", v)
	}
}

func TestEvaluateLabeled_FiresDefaultObserver(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPolicyCollectors(reg)
	policyeval.SetDefaultObserver(c)
	defer policyeval.SetDefaultObserver(nil)

	rules := []policyeval.Rule{{
		Name: "always",
		Then: "yes",
	}}
	if _, err := policyeval.EvaluateLabeled("test-rs", rules, policyeval.Fact{"x": 1}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if v := testutil.ToFloat64(c.fires.WithLabelValues(c.service, "test-rs", "always")); v != 1 {
		t.Fatalf("fires(always) = %v, want 1", v)
	}
}

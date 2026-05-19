package promx

import (
	"github.com/baditaflorin/go-common/policyeval"
	"github.com/prometheus/client_golang/prometheus"
)

// PolicyCollectors records policyeval decisions for the fleet. Wire
// via:
//
//	policyeval.SetDefaultObserver(promx.NewPolicyCollectors(reg))
//	// then call policyeval.EvaluateLabeled("leak-bounty", rules, fact)
//
// Metrics exposed:
//
//	policyeval_evaluations_total{service, ruleset, outcome}
//	policyeval_rule_fires_total{service, ruleset, rule}
//	policyeval_errors_total{service, ruleset}
//
// "outcome" is one of: "matched", "no_match", "error". The fires
// counter answers "which rule wins how often?" — invaluable for
// spotting dead rules (zero fires) or runaway rules (huge fraction
// of traffic) without grepping decision logs.
//
// Cardinality: bounded by (services × rulesets × rule names). All
// three are stable at boot — services declare a fixed []Rule. Empty
// ruleset folds to "_unlabeled" so callers using Evaluate (not
// EvaluateLabeled) still get usable metrics.
type PolicyCollectors struct {
	service string

	evaluations *prometheus.CounterVec
	fires       *prometheus.CounterVec
	errors      *prometheus.CounterVec
}

// NewPolicyCollectors registers the policyeval collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewPolicyCollectors(reg prometheus.Registerer) *PolicyCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &PolicyCollectors{
		service: ServiceID(),
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "policyeval_evaluations_total",
			Help: "Total policyeval.Evaluate calls, labelled by ruleset and outcome.",
		}, []string{"service", "ruleset", "outcome"}),
		fires: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "policyeval_rule_fires_total",
			Help: "Total times each named rule fired within a ruleset.",
		}, []string{"service", "ruleset", "rule"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "policyeval_errors_total",
			Help: "Total evaluation errors (bad regex, type mismatch, unknown operator).",
		}, []string{"service", "ruleset"}),
	}
	reg.MustRegister(c.evaluations, c.fires, c.errors)
	return c
}

// ObservePolicy satisfies policyeval.Observer.
func (c *PolicyCollectors) ObservePolicy(ev policyeval.Event) {
	rs := ev.Ruleset
	if rs == "" {
		rs = "_unlabeled"
	}
	switch {
	case ev.Err != nil:
		c.evaluations.WithLabelValues(c.service, rs, "error").Inc()
		c.errors.WithLabelValues(c.service, rs).Inc()
	case len(ev.Matched) == 0:
		c.evaluations.WithLabelValues(c.service, rs, "no_match").Inc()
	default:
		c.evaluations.WithLabelValues(c.service, rs, "matched").Inc()
		for _, name := range ev.Matched {
			c.fires.WithLabelValues(c.service, rs, name).Inc()
		}
	}
}

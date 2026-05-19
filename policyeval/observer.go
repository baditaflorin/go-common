package policyeval

import "sync/atomic"

// Observer receives one event per Evaluate call. Implementations MUST
// NOT block — callbacks run inline on the decision hot path. The
// canonical implementation lives in go-common/promx.
//
// policyeval deliberately defines the contract here rather than
// importing a metrics library: policyeval keeps zero metric-stack deps.
type Observer interface {
	ObservePolicy(Event)
}

// Event is the per-Evaluate payload handed to an Observer. Matched
// carries every rule that fired (same as Result.Matched); the
// observer can fan it out into per-rule counters.
type Event struct {
	// Ruleset identifies the caller's logical ruleset, e.g.
	// "leak-bounty-policy" or "target-reputation". Empty when the
	// caller used Evaluate without labelling.
	Ruleset string
	// Matched is the ordered list of rule names that fired. Empty
	// when no rule matched.
	Matched []string
	// Winner is the rule whose decision was returned, or "" when no
	// rule matched.
	Winner string
	// Err carries any evaluation error (bad regex, type mismatch).
	// Non-nil errors abort evaluation and Matched/Winner are empty.
	Err error
}

// defaultObserver is the process-wide observer used by Evaluate /
// EvaluateLabeled when no per-call observer is provided. Stored as a
// pointer-to-Observer (an interface value) so atomic.Pointer can hold
// nil cleanly — atomic.Value rejects typed-nil interface stores.
var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer. Pass nil to
// disable. Set by go-common/server.AutoWire when promx is in use so
// every Evaluate call in the process produces fleet metrics.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer or nil.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func emit(ruleset string, res Result, err error) {
	obs := DefaultObserver()
	if obs == nil {
		return
	}
	winner := ""
	if len(res.Matched) > 0 {
		// Last rule in Matched is the winner unless a Stop=true rule
		// fired earlier; in either case it's the final entry.
		winner = res.Matched[len(res.Matched)-1]
	}
	obs.ObservePolicy(Event{
		Ruleset: ruleset,
		Matched: res.Matched,
		Winner:  winner,
		Err:     err,
	})
}

// EvaluateLabeled is Evaluate with an explicit ruleset label that
// flows into the Observer. Use this in production code — it lets the
// promx collector emit `policyeval_rule_fires_total{ruleset,rule}`
// with stable, low-cardinality labels.
func EvaluateLabeled(ruleset string, rules []Rule, fact Fact) (Result, error) {
	res, err := Evaluate(rules, fact)
	emit(ruleset, res, err)
	return res, err
}

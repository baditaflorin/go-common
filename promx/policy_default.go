package promx

import "github.com/baditaflorin/go-common/policyeval"

// setPolicyDefaultObserver wires the PolicyCollectors as the
// process-wide policyeval observer. Split into its own file so the
// import edge from promx → policyeval is grep-able.
func setPolicyDefaultObserver(c *PolicyCollectors) {
	policyeval.SetDefaultObserver(c)
}

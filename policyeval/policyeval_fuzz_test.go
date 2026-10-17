package policyeval_test

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/policyeval"
)

// FuzzEvaluate builds a one-predicate ruleset from fuzzed parts and runs it
// against facts of every supported shape. Bad patterns, unknown operators and
// mismatched kinds must come back as errors, never as panics.
func FuzzEvaluate(f *testing.F) {
	f.Add("s", uint8(0), uint8(0), "x", 1.0, "x", 2.0)
	f.Add("m.n.deep", uint8(3), uint8(1), "", 2.5, "", 3.0)
	f.Add("s", uint8(9), uint8(0), "^(a+)+$", 0.0, "aaaa", 0.0)
	f.Add("l", uint8(8), uint8(0), "x", 0.0, "x", 0.0)
	f.Add("t", uint8(2), uint8(1), "", 1e18, "", -1e18)
	f.Add("", uint8(200), uint8(5), "\x00", 0.0, "\xff", 0.0)
	f.Fuzz(func(t *testing.T, field string, opSel, valKind uint8, valStr string, valNum float64, factStr string, factNum float64) {
		ops := []policyeval.Op{
			policyeval.Eq, policyeval.Ne, policyeval.Lt, policyeval.Le, policyeval.Gt,
			policyeval.Ge, policyeval.In, policyeval.NotIn, policyeval.Contains,
			policyeval.Matches, policyeval.Exists, policyeval.NotExists,
			policyeval.Op(valStr), // an unknown operator
		}
		var val any
		switch valKind % 7 {
		case 0:
			val = valStr
		case 1:
			val = valNum
		case 2:
			val = int(valNum)
		case 3:
			val = []any{valStr, valNum}
		case 4:
			val = []string{valStr}
		case 5:
			val = time.Unix(int64(valNum), 0)
		default:
			val = nil
		}
		rules := []policyeval.Rule{{
			Name: "fuzz",
			When: []policyeval.Predicate{{Field: field, Op: ops[int(opSel)%len(ops)], Value: val}},
			Then: "decision",
		}}
		facts := []policyeval.Fact{
			{
				"s": factStr, "n": factNum, "i": int(factNum), "b": true, "nil": nil,
				"l": []string{factStr}, "a": []any{factStr, factNum},
				"t":  time.Unix(int64(factNum), 0),
				"m":  map[string]any{"k": factStr, "n": map[string]any{"deep": factNum}},
				"st": struct{ A string }{A: factStr},
			},
			{field: factStr},
			nil,
		}
		for _, fact := range facts {
			// Rejecting a rule with an error is fine; panicking is not.
			_, _ = policyeval.Evaluate(rules, fact)
		}
	})
}

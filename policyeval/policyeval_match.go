package policyeval

import (
	"fmt"
	"strings"
)

// matchRule returns (matched, reason, error). reason is a short
// per-rule description like "severity in [critical high] AND env eq
// prod" used to assemble the explanation trail.
func matchRule(r *Rule, fact Fact) (bool, string, error) {
	if len(r.When) == 0 {
		return true, "always", nil
	}
	parts := make([]string, 0, len(r.When))
	for i := range r.When {
		p := &r.When[i]
		ok, err := matchPredicate(p, fact)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "", nil
		}
		parts = append(parts, describePredicate(p))
	}
	return true, strings.Join(parts, " AND "), nil
}

func matchPredicate(p *Predicate, fact Fact) (bool, error) {
	val, found := resolveField(p.Field, fact)
	switch p.Op {
	case Exists:
		return found && val != nil, nil
	case NotExists:
		return !found || val == nil, nil
	}
	if !found {
		// Missing field is "false" for every operator other than
		// NotExists; we already handled NotExists above.
		return false, nil
	}
	switch p.Op {
	case Eq:
		return equalValues(val, p.Value), nil
	case Ne:
		return !equalValues(val, p.Value), nil
	case Lt, Le, Gt, Ge:
		return compareOrdered(val, p.Value, p.Op)
	case In:
		return inSlice(val, p.Value)
	case NotIn:
		ok, err := inSlice(val, p.Value)
		if err != nil {
			return false, err
		}
		return !ok, nil
	case Contains:
		return containsValue(val, p.Value)
	case Matches:
		pat, ok := p.Value.(string)
		if !ok {
			return false, fmt.Errorf("op %q expects string pattern, got %T", p.Op, p.Value)
		}
		s, ok := val.(string)
		if !ok {
			return false, fmt.Errorf("op %q expects string fact, got %T", p.Op, val)
		}
		re, err := compileRegex(pat)
		if err != nil {
			return false, err
		}
		return re.MatchString(s), nil
	default:
		return false, fmt.Errorf("%w: %q", ErrUnknownOp, p.Op)
	}
}

// Package policyeval is a small, pure-Go rule engine used by fleet
// services that need to take a fact (a typed input flattened to a
// map) and a static ruleset, and decide an outcome (channel, priority,
// tags, …) plus a human-readable explanation of which rules fired.
//
// It exists because at least five services in the fleet had each
// rolled their own near-identical "if severity in [critical,high] and
// env eq prod then route to slack-incident" decision engine —
// scope-guard, leak-bounty-policy, target-reputation
// (ADR-0021), fleet-priority-queue (ADR-0019), and finding-triage.
// Centralising the engine means adding a new operator or fact shape
// touches one repo, not five.
//
// Design choices, called out so callers can plan around them:
//
//   - Pure stdlib. No Rego runtime, no YAML loader, no plugin
//     surface. Rules are expressed as Go literals (`[]Rule{…}`) in
//     the service's own code. If you need rules-from-config, decode
//     your config into `[]Rule` yourself and pass it in.
//   - `Decision` is `any` — the engine treats it opaquely. Callers
//     define their own decision struct.
//   - `Fact` is `map[string]any`. Build it from your typed input by
//     hand or via reflection — the engine itself only needs the map.
//   - Dotted field access (`target.severity`) walks
//     `map[string]any` and exported struct fields. A missing
//     intermediate hop makes the predicate false, except for
//     `NotExists`, where it makes the predicate true.
//   - Numeric ordering (`Lt`/`Le`/`Gt`/`Ge`) accepts mixed
//     int / int64 / float64; a `time.Time` on both sides is
//     compared chronologically. Mismatched kinds (string vs int)
//     return an error from `Evaluate` rather than silently failing.
//   - Regexps are compiled on first use and cached by pattern
//     string. Bad patterns surface from `Evaluate` as errors,
//     not panics.
//   - A `[]Rule` is read-only after construction. `Evaluate` is
//     safe to call concurrently against the same ruleset.
package policyeval

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Fact is the input to evaluation: a map keyed on field name. Values
// can be string, bool, int / int64 / float64, time.Time, []string,
// []any, nested map[string]any, or a struct (exported fields only).
// Callers wrap their typed input into a Fact via reflection or by
// hand. The engine does not care how the map was built.
type Fact map[string]any

// Op is the comparison kind in a Predicate.
type Op string

// Supported operators.
const (
	// Eq returns true when the field value equals Value. For slices,
	// equality is element-wise via reflect.DeepEqual.
	Eq Op = "eq"
	// Ne is the negation of Eq.
	Ne Op = "ne"
	// Lt / Le / Gt / Ge compare numerics (int/int64/float64) or
	// chronological time.Time values. Mixed kinds (string vs int)
	// return an error from Evaluate.
	Lt Op = "lt"
	Le Op = "le"
	Gt Op = "gt"
	Ge Op = "ge"
	// In matches when the fact value equals any element of Value.
	// Value must be []any (or a typed slice — the engine converts).
	In Op = "in"
	// NotIn is the negation of In.
	NotIn Op = "not_in"
	// Contains matches:
	//   * substring: fact is string, Value is string
	//   * slice membership: fact is a slice, Value is any element
	Contains Op = "contains"
	// Matches runs Value (a string regexp) against a string fact.
	// Bad patterns return an error from Evaluate.
	Matches Op = "matches"
	// Exists is true when the dotted field resolves to a non-nil value.
	Exists Op = "exists"
	// NotExists is the negation of Exists. Unlike most predicates,
	// it is *true* when an intermediate dotted hop is missing.
	NotExists Op = "not_exists"
)

// Predicate is one term in a Rule's When list.
type Predicate struct {
	// Field is the fact key. Dotted paths walk nested
	// map[string]any / struct values: `target.severity`,
	// `request.headers.x_api_key`.
	Field string
	// Op is the comparison.
	Op Op
	// Value is the right-hand operand. Shape depends on Op (see the
	// per-op docs above). Ignored for Exists / NotExists.
	Value any
}

// Rule is a single decision: a list of predicates joined with AND,
// plus the Decision to emit if every predicate matches. An empty
// When list is treated as always-match.
type Rule struct {
	// Name identifies the rule in the explanation trail. Keep it
	// short and human-readable (`high-severity-prod`,
	// `noisy-source-throttle`).
	Name string
	// When is the AND-joined predicate list. Empty = always match.
	When []Predicate
	// Then is the decision returned when the rule matches.
	Then Decision
	// Stop, when true, halts evaluation after this rule fires. The
	// rule's decision becomes the final one. Without Stop, the last
	// matching rule wins (later rules override earlier ones).
	Stop bool
}

// Decision is opaque to the engine. Callers define their own struct,
// e.g. `type Decision struct{ Channel string; Priority int; Tags []string }`.
type Decision any

// Result is what Evaluate returns.
type Result struct {
	// Decision is the winning rule's Then. Nil interface when no
	// rule matched.
	Decision Decision
	// Matched lists every rule that fired, in evaluation order.
	Matched []string
	// Explanation is a concise human-readable trail. Example:
	//   `matched 2 rule(s): high-sev (severity in [critical high] AND env eq prod); ...`
	// Format is intentionally informal — for logs and `/explain`
	// endpoints, not machine parsing.
	Explanation string
}

// Evaluate runs the ruleset against fact and returns the winning
// decision plus a trail. Selection rules:
//
//   - If no rule matches, Result has zero-value Decision (nil
//     interface), empty Matched, and an empty Explanation; no error.
//   - If a matched rule has Stop=true, evaluation halts there and
//     that rule wins.
//   - Otherwise the LAST matching rule wins.
//
// Evaluation errors (bad regex, type mismatch in ordered compare,
// unknown operator) abort evaluation and are returned as the error
// value. The caller decides whether to treat construction errors
// as fail-open or fail-closed.
//
// Evaluate is safe to call from multiple goroutines against the same
// ruleset.
func Evaluate(rules []Rule, fact Fact) (Result, error) {
	var (
		res     Result
		winner  *Rule
		reasons []string
	)
	for i := range rules {
		r := &rules[i]
		ok, reason, err := matchRule(r, fact)
		if err != nil {
			return Result{}, fmt.Errorf("policyeval: rule %q: %w", r.Name, err)
		}
		if !ok {
			continue
		}
		res.Matched = append(res.Matched, r.Name)
		reasons = append(reasons, fmt.Sprintf("%s (%s)", r.Name, reason))
		winner = r
		if r.Stop {
			break
		}
	}
	if winner != nil {
		res.Decision = winner.Then
		res.Explanation = fmt.Sprintf(
			"matched %d rule(s): %s",
			len(res.Matched),
			strings.Join(reasons, "; "),
		)
	}
	return res, nil
}

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

func describePredicate(p *Predicate) string {
	switch p.Op {
	case Exists, NotExists:
		return fmt.Sprintf("%s %s", p.Field, p.Op)
	default:
		return fmt.Sprintf("%s %s %v", p.Field, p.Op, p.Value)
	}
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

// resolveField walks a dotted path into fact. Returns
// (value, found). found=false means an intermediate hop was missing
// or a leaf was absent.
func resolveField(path string, fact Fact) (any, bool) {
	if path == "" {
		return nil, false
	}
	var cur any = map[string]any(fact)
	for _, seg := range strings.Split(path, ".") {
		next, ok := step(cur, seg)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func step(cur any, seg string) (any, bool) {
	if cur == nil {
		return nil, false
	}
	switch m := cur.(type) {
	case map[string]any:
		v, ok := m[seg]
		return v, ok
	case Fact:
		v, ok := m[seg]
		return v, ok
	}
	rv := reflect.ValueOf(cur)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		// Map with non-string key types: rare in fact shapes; treat
		// as missing rather than guessing a conversion.
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		v := rv.MapIndex(reflect.ValueOf(seg))
		if !v.IsValid() {
			return nil, false
		}
		return v.Interface(), true
	case reflect.Struct:
		f := rv.FieldByName(seg)
		if !f.IsValid() || !f.CanInterface() {
			return nil, false
		}
		return f.Interface(), true
	}
	return nil, false
}

func equalValues(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	if at, aok := a.(time.Time); aok {
		if bt, bok := b.(time.Time); bok {
			return at.Equal(bt)
		}
	}
	return reflect.DeepEqual(a, b)
}

// compareOrdered implements Lt/Le/Gt/Ge across int/int64/float64 and
// time.Time. Mixed kinds (one numeric, one string) return an error.
func compareOrdered(a, b any, op Op) (bool, error) {
	if at, aok := a.(time.Time); aok {
		bt, bok := b.(time.Time)
		if !bok {
			return false, fmt.Errorf("op %q: cannot compare time.Time to %T", op, b)
		}
		return orderedTime(at, bt, op), nil
	}
	if _, ok := b.(time.Time); ok {
		return false, fmt.Errorf("op %q: cannot compare %T to time.Time", op, a)
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if !aok || !bok {
		return false, fmt.Errorf("op %q: non-numeric comparison %T vs %T", op, a, b)
	}
	switch op {
	case Lt:
		return af < bf, nil
	case Le:
		return af <= bf, nil
	case Gt:
		return af > bf, nil
	case Ge:
		return af >= bf, nil
	}
	return false, fmt.Errorf("compareOrdered: unhandled op %q", op)
}

func orderedTime(a, b time.Time, op Op) bool {
	switch op {
	case Lt:
		return a.Before(b)
	case Le:
		return !a.After(b)
	case Gt:
		return a.After(b)
	case Ge:
		return !a.Before(b)
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// inSlice returns true when fact value equals any element of the
// expected slice (typically []any but typed slices work via reflect).
func inSlice(fact, expected any) (bool, error) {
	rv := reflect.ValueOf(expected)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return false, fmt.Errorf("op in/not_in: expected []any (or slice), got %T", expected)
	}
	for i := 0; i < rv.Len(); i++ {
		if equalValues(fact, rv.Index(i).Interface()) {
			return true, nil
		}
	}
	return false, nil
}

// containsValue covers two distinct cases — substring search when the
// fact is a string, slice-membership when the fact is a slice.
func containsValue(fact, needle any) (bool, error) {
	if s, ok := fact.(string); ok {
		n, ok := needle.(string)
		if !ok {
			return false, fmt.Errorf("op contains: string fact needs string needle, got %T", needle)
		}
		return strings.Contains(s, n), nil
	}
	rv := reflect.ValueOf(fact)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		for i := 0; i < rv.Len(); i++ {
			if equalValues(rv.Index(i).Interface(), needle) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("op contains: fact must be string or slice, got %T", fact)
}

// --- regex cache -----------------------------------------------------

var (
	regexCacheMu sync.RWMutex
	regexCache   = map[string]*regexp.Regexp{}
)

// compileRegex returns a cached compiled regexp for pattern, compiling
// on first use. Concurrent callers race for the write lock but are
// idempotent — they all end up with the same compiled value.
func compileRegex(pattern string) (*regexp.Regexp, error) {
	regexCacheMu.RLock()
	if re, ok := regexCache[pattern]; ok {
		regexCacheMu.RUnlock()
		return re, nil
	}
	regexCacheMu.RUnlock()

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad regex %q: %w", pattern, err)
	}
	regexCacheMu.Lock()
	regexCache[pattern] = re
	regexCacheMu.Unlock()
	return re, nil
}

// --- sentinel errors -------------------------------------------------

// ErrUnknownOp is returned (wrapped) when a Predicate carries an Op
// that the engine does not implement. Callers can `errors.Is` against
// it if they want to distinguish from data-shape errors.
var ErrUnknownOp = errors.New("policyeval: unknown operator")

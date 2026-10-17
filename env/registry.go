package env

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A package that reads environment variables can declare them here, so that the
// process can answer questions nothing could before: what knobs exist, what do
// they default to, which of the ones that are set are mistyped, are any of them
// gone. The registry is a catalogue plus validation; reading and interpreting
// a value stays with the package that owns it (many have rules the generic
// readers in this package do not, such as "a ratio strictly between 0 and 1").
//
//	var envInterval = env.Declare(env.Var{
//	    Name: "FLEET_RUNTIME_TUNING_INTERVAL", Kind: env.KindDuration, Default: "5s",
//	    Doc: "How often the runtime tuner re-evaluates GC settings.",
//	})
//	...
//	raw := envInterval.Value() // os.Getenv, by a name that cannot be misspelled
//
// Declare at package level, so declarations exist before main runs.

// Kind is what a variable's value must parse as.
type Kind string

// Kinds.
const (
	KindString   Kind = "string"
	KindBool     Kind = "bool"
	KindInt      Kind = "int"
	KindFloat    Kind = "float"
	KindDuration Kind = "duration"
	KindList     Kind = "list" // comma-separated
)

// Var is one declared environment variable.
type Var struct {
	Name    string
	Kind    Kind
	Default string // as the user would write it; shown in docs, never for a secret
	Doc     string
	// Secret hides the value and the default everywhere the registry shows them.
	// Declare every credential as Secret. Values that look sensitive are also
	// hidden without it (see Redact), as a second line, not a substitute.
	Secret bool
	// Check validates a value that is set and has already parsed as Kind, for
	// rules beyond the kind ("a ratio in (0,1)").
	Check func(value string) error

	owner string
}

// Value returns the variable's value, "" if unset: os.Getenv by declared name.
func (v *Var) Value() string { return os.Getenv(v.Name) }

// Lookup is os.LookupEnv by declared name.
func (v *Var) Lookup() (string, bool) { return os.LookupEnv(v.Name) }

// Owner is the package that declared the variable.
func (v *Var) Owner() string { return v.owner }

type removal struct{ name, since, advice string }

// Registry holds declarations. Default is the process-wide one.
type Registry struct {
	mu        sync.Mutex
	vars      map[string]*Var
	removed   map[string]removal
	conflicts []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{vars: map[string]*Var{}, removed: map[string]removal{}}
}

// Default is the registry packages declare into.
var Default = NewRegistry()

var nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Declare registers v in the Default registry and returns the stored variable.
func Declare(v Var) *Var { return Default.declare(v, 3) }

// Declare registers v and returns the stored variable. Declaring the same
// variable twice with the same kind, default and secrecy returns the first; a
// different redeclaration keeps the first and is reported by Validate.
func (r *Registry) Declare(v Var) *Var { return r.declare(v, 3) }

func (r *Registry) declare(v Var, skip int) *Var {
	v.owner = callerPackage(skip)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !nameRe.MatchString(v.Name) {
		r.conflicts = append(r.conflicts, fmt.Sprintf("%s declared an invalid variable name %q (upper-case letters, digits and '_')", v.owner, v.Name))
	}
	if v.Kind == "" {
		v.Kind = KindString
	}
	if have, ok := r.vars[v.Name]; ok {
		if have.Kind != v.Kind || have.Default != v.Default || have.Secret != v.Secret {
			r.conflicts = append(r.conflicts, fmt.Sprintf("%s is declared by %s and again by %s with a different kind, default or secrecy", v.Name, have.owner, v.owner))
		}
		return have
	}
	stored := v
	r.vars[v.Name] = &stored
	return &stored
}

// Removal is the record of a variable that no longer does anything.
type Removal struct{ Name, Since, Advice string }

// Removed records, in the Default registry, that name no longer does anything. If
// it is still set in a process's environment, Validate says so: nobody can
// otherwise find out who still sets a variable that was removed. It returns the
// record so it can be declared at package level without an init function:
//
//	var _ = env.Removed("OLD_SWITCH", "v0.102.60", "delete it")
func Removed(name, since, advice string) Removal { return Default.Removed(name, since, advice) }

// RemovedList returns the removed variables recorded in the registry, sorted by name.
func (r *Registry) RemovedList() []Removal {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Removal, 0, len(r.removed))
	for _, rm := range r.removed {
		out = append(out, Removal{rm.name, rm.since, rm.advice})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Removed records that name was removed in release since; advice says what to do.
func (r *Registry) Removed(name, since, advice string) Removal {
	rm := Removal{name, since, advice}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed[name] = removal{name, since, advice}
	return rm
}

// callerPackage returns the import path of the package skip frames up, relative to
// this module, for the Owner column.
func callerPackage(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "?"
	}
	name := runtime.FuncForPC(pc).Name() // e.g. github.com/baditaflorin/go-common/graph.init
	if i := strings.LastIndex(name, "/"); i >= 0 {
		if j := strings.Index(name[i:], "."); j >= 0 {
			name = name[:i+j]
		}
	}
	return strings.TrimPrefix(name, "github.com/baditaflorin/go-common/")
}

// Problem is something Validate found.
type Problem struct {
	Name    string `json:"name"`
	Level   string `json:"level"` // "error": a set value that cannot work; "warning": worth a look
	Message string `json:"message"`
}

// Validate checks environ (KEY=VALUE strings, as os.Environ returns) against the
// declarations:
//   - a declared variable that is set must parse as its Kind and pass its Check;
//   - a removed variable that is still set gets a warning;
//   - a set variable that is not declared but is one or two edits away from a
//     declared name that shares its first word (GRAPH_COLLECTER_URL for
//     GRAPH_COLLECTOR_URL) gets a warning with the suggestion: a typo is
//     otherwise silent, because the real variable just stays unset;
//   - conflicting declarations are reported.
//
// An empty value counts as unset, like the readers in this package.
func (r *Registry) Validate(environ []string) []Problem {
	set := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			set[k] = v
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Problem
	for _, c := range r.conflicts {
		out = append(out, Problem{Level: "warning", Message: c})
	}

	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		val := set[name]
		if val == "" {
			continue
		}
		if v, ok := r.vars[name]; ok {
			if err := v.Validate(val); err != nil {
				out = append(out, Problem{Name: name, Level: "error", Message: fmt.Sprintf("%s=%s: %v", name, shown(v, val), err)})
			}
			continue
		}
		if rm, ok := r.removed[name]; ok {
			out = append(out, Problem{Name: name, Level: "warning", Message: fmt.Sprintf("%s was removed in %s and is ignored: %s", name, rm.since, rm.advice)})
			continue
		}
		if guess := r.closest(name); guess != "" {
			out = append(out, Problem{Name: name, Level: "warning", Message: fmt.Sprintf("%s is not a known variable; did you mean %s?", name, guess)})
		}
	}
	return out
}

// shown renders a value for a message: never a secret's.
func shown(v *Var, val string) string {
	if v.Secret || looksSensitive(v.Name, val) {
		return "[redacted]"
	}
	return strconv.Quote(val)
}

// Validate reports whether val is acceptable for the variable: it must parse as
// Kind and pass Check. Registry.Validate applies it to every variable that is set.
func (v *Var) Validate(val string) error {
	switch v.Kind {
	case KindBool:
		switch strings.ToLower(val) {
		case "1", "true", "yes", "on", "0", "false", "no", "off":
		default:
			return fmt.Errorf("not a boolean (use true or false)")
		}
	case KindInt:
		if _, err := strconv.Atoi(val); err != nil {
			return fmt.Errorf("not an integer")
		}
	case KindFloat:
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return fmt.Errorf("not a number")
		}
	case KindDuration:
		if _, err := time.ParseDuration(val); err != nil {
			return fmt.Errorf("not a duration such as 5s or 2m")
		}
	}
	if v.Check != nil {
		return v.Check(val)
	}
	return nil
}

// closest returns the declared name that name is likely a typo of, or "".
func (r *Registry) closest(name string) string {
	word, _, _ := strings.Cut(name, "_")
	best, bestDist := "", 3
	for cand := range r.vars {
		if w, _, _ := strings.Cut(cand, "_"); w != word {
			continue
		}
		if d := distance(name, cand); d < bestDist || (d == bestDist && cand < best) {
			best, bestDist = cand, d
		}
	}
	if bestDist <= 2 {
		return best
	}
	return ""
}

// distance is the Levenshtein edit distance.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Check validates the process's own environment, logs each problem and returns
// how many were errors. It never exits: a mistyped optional variable must not
// take a service down. Call it once at start-up.
func (r *Registry) Check() int {
	errs := 0
	for _, p := range r.Validate(os.Environ()) {
		if p.Level == "error" {
			errs++
		}
		log.Printf("env: %s: %s", p.Level, p.Message)
	}
	return errs
}

// Info describes one variable for a listing.
type Info struct {
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	Default string `json:"default,omitempty"`
	Doc     string `json:"doc,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Secret  bool   `json:"secret,omitempty"`
	Set     bool   `json:"set"`
	Value   string `json:"value,omitempty"` // redacted for anything sensitive
}

// Redacted replaces a sensitive value.
const Redacted = "[redacted]"

var sensitiveName = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASS|PWD|CREDENTIAL|AUTH|COOKIE|PRIVATE|DSN|DATASOURCE)`)
var userinfoURL = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]*@`)

// looksSensitive is the second line of defence: a name or value that looks like a
// credential is hidden even if its declaration forgot Secret.
func looksSensitive(name, value string) bool {
	return sensitiveName.MatchString(name) || userinfoURL.MatchString(value)
}

// Snapshot lists every declared variable, sorted by name, with its value taken
// from environ. Sensitive values are redacted.
func (r *Registry) Snapshot(environ []string) []Info {
	set := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set[k] = v
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Info, 0, len(r.vars))
	for _, v := range r.vars {
		val, isSet := set[v.Name]
		isSet = isSet && val != ""
		info := Info{Name: v.Name, Kind: v.Kind, Default: v.Default, Doc: v.Doc, Owner: v.owner, Secret: v.Secret, Set: isSet}
		sensitive := v.Secret || looksSensitive(v.Name, val)
		if v.Secret {
			info.Default = ""
		}
		if isSet {
			if sensitive {
				info.Value = Redacted
			} else {
				info.Value = val
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Handler serves the registry as JSON: every declared variable with its redacted
// value, and the problems Validate finds. It is for a debug listener that is not
// reachable from outside (obs mounts it on its localhost-only server), never for a
// public route.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		environ := os.Environ()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Vars     []Info    `json:"vars"`
			Problems []Problem `json:"problems"`
		}{r.Snapshot(environ), r.Validate(environ)})
	})
}

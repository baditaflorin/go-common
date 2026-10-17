package env

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decl(r *Registry) {
	r.Declare(Var{Name: "GRAPH_COLLECTOR_URL", Kind: KindString, Doc: "collector"})
	r.Declare(Var{Name: "GRAPH_ENABLED", Kind: KindBool, Default: "false"})
	r.Declare(Var{Name: "GRAPH_FLUSH_INTERVAL", Kind: KindInt, Default: "10"})
	r.Declare(Var{Name: "GRAPH_SAMPLE_RATE", Kind: KindFloat, Default: "1.0"})
	r.Declare(Var{Name: "GRAPH_TIMEOUT", Kind: KindDuration, Default: "5s"})
	r.Declare(Var{Name: "GRAPH_API_KEY", Kind: KindString, Secret: true, Default: "hunter2"})
	r.Declare(Var{Name: "GRAPH_RATIO", Kind: KindFloat, Check: func(v string) error {
		if v == "2" {
			return errors.New("must be below 1")
		}
		return nil
	}})
}

func find(ps []Problem, name string) *Problem {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

func TestValidateAcceptsGoodValuesAndIgnoresUnset(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	decl(r)
	got := r.Validate([]string{
		"GRAPH_ENABLED=true", "GRAPH_FLUSH_INTERVAL=30", "GRAPH_SAMPLE_RATE=0.25", "GRAPH_TIMEOUT=250ms",
		"GRAPH_COLLECTOR_URL=http://c:1", "GRAPH_API_KEY=k", "GRAPH_RATIO=0.5", "PATH=/bin", "GRAPH_ENABLED_EMPTY=",
	})
	if len(got) != 0 {
		t.Fatalf("problems for a valid environment: %+v", got)
	}
	// An empty value is "unset", like the readers: it is not validated.
	if got := r.Validate([]string{"GRAPH_FLUSH_INTERVAL="}); len(got) != 0 {
		t.Fatalf("an empty value was validated: %+v", got)
	}
}

func TestValidateFlagsValuesThatCannotParse(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	decl(r)
	for name, val := range map[string]string{
		"GRAPH_ENABLED":        "ture",
		"GRAPH_FLUSH_INTERVAL": "ten",
		"GRAPH_SAMPLE_RATE":    "half",
		"GRAPH_TIMEOUT":        "5 seconds",
		"GRAPH_RATIO":          "2", // parses, fails its Check
	} {
		ps := r.Validate([]string{name + "=" + val})
		p := find(ps, name)
		if p == nil || p.Level != "error" || !strings.Contains(p.Message, name) {
			t.Errorf("%s=%s: problems = %+v, want an error naming the variable", name, val, ps)
		}
	}
	if p := find(r.Validate([]string{"GRAPH_RATIO=2"}), "GRAPH_RATIO"); p == nil || !strings.Contains(p.Message, "below 1") {
		t.Errorf("a Check failure should say why: %+v", p)
	}
}

func TestValidateNeverEchoesASecretValue(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Declare(Var{Name: "VAULT_TOKEN", Kind: KindInt, Secret: true})
	r.Declare(Var{Name: "PLAIN_NUM", Kind: KindInt})
	r.Declare(Var{Name: "SEARCH_URL", Kind: KindBool}) // not marked secret, but the value carries credentials
	bad := r.Validate([]string{"VAULT_TOKEN=s3cr3t-value", "PLAIN_NUM=plain-value", "SEARCH_URL=https://user:p4ss@host/x"})
	var joined string
	for _, p := range bad {
		joined += p.Message + "\n"
	}
	if strings.Contains(joined, "s3cr3t-value") || strings.Contains(joined, "p4ss") {
		t.Fatalf("a sensitive value reached a message:\n%s", joined)
	}
	if !strings.Contains(joined, "plain-value") {
		t.Fatalf("an ordinary value should be shown to help the operator:\n%s", joined)
	}
}

func TestRemovedVariablesAreReportedWhileStillSet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	decl(r)
	r.Removed("GRAPH_USE_EGRESS_PROXY", "v0.102.60", "graph is always direct; delete it")
	p := find(r.Validate([]string{"GRAPH_USE_EGRESS_PROXY=true"}), "GRAPH_USE_EGRESS_PROXY")
	if p == nil || p.Level != "warning" || !strings.Contains(p.Message, "v0.102.60") || !strings.Contains(p.Message, "delete it") {
		t.Fatalf("problem = %+v", p)
	}
	if got := r.Validate([]string{"GRAPH_USE_EGRESS_PROXY="}); len(got) != 0 {
		t.Fatalf("a removed variable that is empty (unset) was reported: %+v", got)
	}
}

func TestTyposNextToDeclaredNamesAreSuggested(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	decl(r)
	for typo, want := range map[string]string{
		"GRAPH_COLLECTER_URL": "GRAPH_COLLECTOR_URL",
		"GRAPH_ENABLE":        "GRAPH_ENABLED",
		"GRAPH_API_KYE":       "GRAPH_API_KEY",
	} {
		p := find(r.Validate([]string{typo + "=x"}), typo)
		if p == nil || !strings.Contains(p.Message, "did you mean "+want) {
			t.Errorf("%s: problem = %+v, want a suggestion of %s", typo, p, want)
		}
	}
	for _, quiet := range []string{
		"PATH",                 // a different first word
		"GRAPH_SOMETHING_ELSE", // same first word but far from any declared name
		"HOME",
	} {
		if p := find(r.Validate([]string{quiet + "=x"}), quiet); p != nil {
			t.Errorf("%s was flagged: %+v", quiet, p)
		}
	}
}

func TestDeclareIsIdempotentAndReportsConflicts(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	a := r.Declare(Var{Name: "X_ONE", Kind: KindInt, Default: "1"})
	b := r.Declare(Var{Name: "X_ONE", Kind: KindInt, Default: "1"})
	if a != b {
		t.Fatal("an identical redeclaration should return the first")
	}
	if got := r.Validate(nil); len(got) != 0 {
		t.Fatalf("an identical redeclaration was reported: %+v", got)
	}
	r.Declare(Var{Name: "X_ONE", Kind: KindString, Default: "other"})
	r.Declare(Var{Name: "lower-case", Kind: KindString})
	got := r.Validate(nil)
	if len(got) != 2 {
		t.Fatalf("problems = %+v, want a conflict and an invalid name", got)
	}
}

func TestSnapshotRedactsSecretsAndAnythingSensitive(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	decl(r)
	r.Declare(Var{Name: "UPSTREAM_PROXY", Kind: KindString})
	environ := []string{"GRAPH_API_KEY=live-key", "GRAPH_ENABLED=true", "UPSTREAM_PROXY=http://u:pw@proxy:3128", "GRAPH_TIMEOUT=9s"}
	infos := map[string]Info{}
	for _, i := range r.Snapshot(environ) {
		infos[i.Name] = i
	}
	if i := infos["GRAPH_API_KEY"]; i.Value != Redacted || i.Default != "" || !i.Secret || !i.Set {
		t.Fatalf("secret = %+v: value and default must be hidden", i)
	}
	if i := infos["UPSTREAM_PROXY"]; i.Value != Redacted {
		t.Fatalf("a value with credentials in it was shown: %+v", i)
	}
	if i := infos["GRAPH_ENABLED"]; i.Value != "true" || !i.Set {
		t.Fatalf("an ordinary value should be shown: %+v", i)
	}
	if i := infos["GRAPH_FLUSH_INTERVAL"]; i.Set || i.Value != "" || i.Default != "10" {
		t.Fatalf("an unset variable shows its default only: %+v", i)
	}
	for i := 1; i < len(r.Snapshot(nil)); i++ {
		if r.Snapshot(nil)[i-1].Name >= r.Snapshot(nil)[i].Name {
			t.Fatal("Snapshot is not sorted by name")
		}
	}
}

func TestHandlerServesRedactedJSONAndOnlyGET(t *testing.T) {
	r := NewRegistry()
	decl(r)
	t.Setenv("GRAPH_API_KEY", "super-secret-handler-value")
	t.Setenv("GRAPH_ENABLED", "maybe") // invalid: shows up as a problem

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/config", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code=%d headers=%v", rec.Code, rec.Header())
	}
	if strings.Contains(rec.Body.String(), "super-secret-handler-value") {
		t.Fatalf("the handler leaked a secret: %s", rec.Body.String())
	}
	var body struct {
		Vars     []Info    `json:"vars"`
		Problems []Problem `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Vars) == 0 || find(body.Problems, "GRAPH_ENABLED") == nil {
		t.Fatalf("body = %+v", body)
	}

	rec = httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

func TestValueAndLookupReadTheDeclaredName(t *testing.T) {
	r := NewRegistry()
	v := r.Declare(Var{Name: "REGISTRY_TEST_VALUE", Kind: KindString})
	if got, ok := v.Lookup(); ok || got != "" || v.Value() != "" {
		t.Fatalf("unset: %q %v", got, ok)
	}
	t.Setenv("REGISTRY_TEST_VALUE", "set")
	if got, ok := v.Lookup(); !ok || got != "set" || v.Value() != "set" {
		t.Fatalf("set: %q %v", got, ok)
	}
}

func TestOwnerIsTheDeclaringPackage(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if got := r.Declare(Var{Name: "OWNER_TEST"}).Owner(); got != "env" {
		t.Fatalf("Owner = %q, want env (this test's package)", got)
	}
}

func TestDistance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"a", "", 1}, {"kitten", "sitting", 3}, {"GRAPH_ENABLED", "GRAPH_ENABLE", 1}, {"same", "same", 0}} {
		if got := distance(tc.a, tc.b); got != tc.want {
			t.Errorf("distance(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

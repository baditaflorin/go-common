package compat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T) Manifest {
	t.Helper()
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEmbeddedManifestIsConsistent(t *testing.T) {
	t.Parallel()
	m := mustLoad(t)
	if len(m.Reasons) == 0 {
		t.Fatal("manifest has no reasons")
	}
	newest := version{}
	for _, r := range m.Reasons {
		v, err := parseVersion(r.Version)
		if err != nil {
			t.Fatalf("reason %q: %v", r.Version, err)
		}
		if r.Summary == "" {
			t.Errorf("reason %s has no summary", r.Version)
		}
		if v.compareCore(newest) > 0 {
			newest = v
		}
	}
	floor, _ := parseVersion(m.MinSafe)
	if floor.compareCore(newest) != 0 {
		t.Fatalf("min_safe %s must equal the newest fix in reasons (%d.%d.%d)", m.MinSafe, newest.major, newest.minor, newest.patch)
	}
	if m.MinSafeDate.After(time.Now()) {
		t.Fatalf("min_safe_date %v is in the future", m.MinSafeDate)
	}
}

func TestParseRejectsBadManifests(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"not json":       `{`,
		"wrong schema":   `{"schema":2,"min_safe":"v1.0.0","min_safe_date":"2026-01-01T00:00:00Z"}`,
		"bad min_safe":   `{"schema":1,"min_safe":"banana","min_safe_date":"2026-01-01T00:00:00Z"}`,
		"missing date":   `{"schema":1,"min_safe":"v1.0.0"}`,
		"empty min_safe": `{"schema":1,"min_safe":"","min_safe_date":"2026-01-01T00:00:00Z"}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, doc)
		}
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	m := Manifest{
		Schema:      1,
		MinSafe:     "v0.102.2",
		MinSafeDate: time.Date(2026, 9, 27, 15, 3, 6, 0, time.UTC),
		Reasons:     []Reason{{Version: "v0.102.2", Summary: "patch a gRPC vulnerability"}},
	}
	for _, tc := range []struct {
		ver  string
		want Status
	}{
		{"v0.102.2", StatusOK},
		{"v0.102.34", StatusOK},
		{"v0.103.0", StatusOK},
		{"v1.0.0", StatusOK},
		{"v0.102.1", StatusBelowFloor},
		{"v0.97.0", StatusBelowFloor},
		{"v0.88.0", StatusBelowFloor},
		{"v0.9.9", StatusBelowFloor},
		{"v0.102.2-rc.1", StatusBelowFloor}, // a pre-release sorts below its release
		{"v0.102.3-rc.1", StatusOK},
		{"v0.102.2+incompatible", StatusOK},
		// pseudo-versions are placed by commit time
		{"v0.0.0-20260110171042-8921433476a9", StatusBelowFloor},
		{"v0.0.0-20260927150305-aaaaaaaaaaaa", StatusBelowFloor}, // one second before the floor
		{"v0.0.0-20260927150306-aaaaaaaaaaaa", StatusOK},         // at the floor's commit time
		{"v0.0.0-20261001000000-aaaaaaaaaaaa", StatusOK},
		{"v0.102.3-0.20260920000000-abcdefabcdef", StatusBelowFloor},
		{"v0.102.3-0.20261002000000-abcdefabcdef", StatusOK},
	} {
		res, err := m.Check(tc.ver)
		if err != nil {
			t.Errorf("Check(%s): %v", tc.ver, err)
			continue
		}
		if res.Status != tc.want {
			t.Errorf("Check(%s) = %s (%s), want %s", tc.ver, res.Status, res.Message, tc.want)
		}
	}
	for _, bad := range []string{"", "banana", "0.102.2", "v1", "v1.2", "master"} {
		if _, err := m.Check(bad); err == nil {
			t.Errorf("Check(%q) accepted an invalid version", bad)
		}
	}
}

func TestLinesJudgeABackportedLineByItsOwnFloor(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(`{"schema":1,"min_safe":"v0.102.2","min_safe_date":"2026-09-27T15:03:06Z",
		"reasons":[{"version":"v0.102.2","summary":"x"}],"lines":{"v0.97":"v0.97.1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for ver, want := range map[string]Status{
		"v0.97.0": StatusBelowFloor, // before the backport
		"v0.97.1": StatusOK,         // has it
		"v0.97.5": StatusOK,
		"v0.98.0": StatusBelowFloor, // another old line, no backports: the main floor applies
		"v0.88.0": StatusBelowFloor,
	} {
		res, err := m.Check(ver)
		if err != nil || res.Status != want {
			t.Errorf("Check(%s) = %+v, %v, want %s", ver, res, err, want)
		}
	}
	if res, _ := m.Check("v0.97.1"); !strings.Contains(res.Message, "v0.102.2") {
		t.Errorf("a backported pin should be told the newest floor: %q", res.Message)
	}
}

func TestParseRejectsMalformedLines(t *testing.T) {
	t.Parallel()
	base := `{"schema":1,"min_safe":"v0.102.2","min_safe_date":"2026-09-27T15:03:06Z","lines":%s}`
	for _, lines := range []string{
		`{"v0.97":"banana"}`,
		`{"v0.97":"v0.98.1"}`,    // value is on another line
		`{"0.97":"v0.97.1"}`,     // key is not vMAJOR.MINOR
		`{"v0.97":"v0.97.1-rc"}`, // a floor must be a release
	} {
		if _, err := Parse([]byte(strings.Replace(base, "%s", lines, 1))); err == nil {
			t.Errorf("Parse accepted lines %s", lines)
		}
	}
}

func TestCheckExplainsWhatABelowFloorPinIsMissing(t *testing.T) {
	t.Parallel()
	res, err := mustLoad(t).Check("v0.1.0")
	if err != nil || res.Status != StatusBelowFloor {
		t.Fatalf("Check(v0.1.0) = %+v, %v", res, err)
	}
	if !strings.Contains(res.Message, "gRPC") {
		t.Fatalf("message %q should name the fixes the pin lacks", res.Message)
	}
}

func TestCheckGoMod(t *testing.T) {
	t.Parallel()
	m := mustLoad(t)
	for _, tc := range []struct {
		name string
		mod  string
		want Status
	}{
		{"single-line require, old", "module x\n\ngo 1.25\n\nrequire github.com/baditaflorin/go-common v0.97.0\n", StatusBelowFloor},
		{"block require, new", "module x\n\nrequire (\n\tgithub.com/other/thing v1.0.0\n\tgithub.com/baditaflorin/go-common v0.102.34\n)\n", StatusOK},
		{"indirect marker and comment", "module x\n\nrequire (\n\tgithub.com/baditaflorin/go-common v0.88.0 // indirect\n)\n", StatusBelowFloor},
		{"pseudo-version", "module x\n\nrequire github.com/baditaflorin/go-common v0.0.0-20260110171042-8921433476a9\n", StatusBelowFloor},
		{"local replace", "module x\n\nrequire github.com/baditaflorin/go-common v0.97.0\n\nreplace github.com/baditaflorin/go-common => ../go-common\n", StatusUnchecked},
		{"replace with a version", "module x\n\nrequire github.com/baditaflorin/go-common v0.97.0\n\nreplace github.com/baditaflorin/go-common v0.97.0 => github.com/fork/go-common v0.97.1\n", StatusUnchecked},
		{"not used", "module x\n\nrequire github.com/other/thing v1.0.0\n", StatusNotUsed},
		{"a different module with a similar prefix", "module x\n\nrequire github.com/baditaflorin/go-common-extras v0.1.0\n", StatusNotUsed},
	} {
		res, err := m.CheckGoMod([]byte(tc.mod))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if res.Status != tc.want {
			t.Errorf("%s: status = %s (%s), want %s", tc.name, res.Status, res.Message, tc.want)
		}
	}
}

const unchangedDoc = `{"schema":1,"min_safe":"v0.102.2","min_safe_date":"2026-09-27T15:03:06Z",
	"reasons":[{"version":"v0.102.2","summary":"x"}],
	"min_safe_unchanged":[{"version":"v0.106.0","reason":"a latent hole"}]}`

func TestMinSafeUnchangedParses(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(unchangedDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.MinSafeUnchanged) != 1 || m.MinSafeUnchanged[0].Version != "v0.106.0" || m.MinSafeUnchanged[0].Reason != "a latent hole" {
		t.Fatalf("got %+v", m.MinSafeUnchanged)
	}
	// The record changes no verdict.
	for ver, want := range map[string]Status{"v0.102.1": StatusBelowFloor, "v0.102.2": StatusOK, "v0.106.0": StatusOK} {
		if res, err := m.Check(ver); err != nil || res.Status != want {
			t.Errorf("Check(%s) = %+v, %v, want %s", ver, res, err, want)
		}
	}
}

func TestParseRejectsBadMinSafeUnchanged(t *testing.T) {
	t.Parallel()
	base := `{"schema":1,"min_safe":"v0.102.2","min_safe_date":"2026-09-27T15:03:06Z","min_safe_unchanged":%s}`
	for name, recs := range map[string]string{
		"not a version":    `[{"version":"banana","reason":"r"}]`,
		"a pre-release":    `[{"version":"v0.106.0-rc.1","reason":"r"}]`,
		"no v":             `[{"version":"0.106.0","reason":"r"}]`,
		"no reason":        `[{"version":"v0.106.0"}]`,
		"a blank reason":   `[{"version":"v0.106.0","reason":"  "}]`,
		"the same release": `[{"version":"v0.106.0","reason":"a"},{"version":"v0.106.0","reason":"b"}]`,
	} {
		if _, err := Parse([]byte(strings.Replace(base, "%s", recs, 1))); err == nil {
			t.Errorf("%s: Parse accepted %s", name, recs)
		}
	}
}

// The manifest has to stay readable by the releases of this package that predate the
// record: they decode it with encoding/json into a Manifest that has no such field.
func TestMinSafeUnchangedIsInvisibleToAnOlderReader(t *testing.T) {
	t.Parallel()
	type oldManifest struct {
		Schema      int       `json:"schema"`
		MinSafe     string    `json:"min_safe"`
		MinSafeDate time.Time `json:"min_safe_date"`
		Reasons     []Reason  `json:"reasons"`
		Note        string    `json:"note"`
	}
	var old oldManifest
	if err := json.Unmarshal([]byte(unchangedDoc), &old); err != nil {
		t.Fatalf("an older reader fails on the record: %v", err)
	}
	if old.MinSafe != "v0.102.2" || len(old.Reasons) != 1 {
		t.Fatalf("an older reader read %+v", old)
	}
}

func TestFloorDecision(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(unchangedDoc))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ver    string
		status FloorStatus
		reason string
	}{
		{"v0.80.0", FloorCovers, ""},
		{"v0.102.2", FloorCovers, ""}, // the floor itself
		{"v0.102.3", FloorUndecided, ""},
		{"v0.106.0", FloorUnchanged, "a latent hole"},
		{"v0.106.1", FloorUndecided, ""},
	} {
		st, reason, err := m.FloorDecision(tc.ver)
		if err != nil || st != tc.status || reason != tc.reason {
			t.Errorf("FloorDecision(%s) = %s, %q, %v; want %s, %q", tc.ver, st, reason, err, tc.status, tc.reason)
		}
	}
	if _, _, err := m.FloorDecision("banana"); err == nil {
		t.Error("FloorDecision accepted banana")
	}
}

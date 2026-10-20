package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/compat"
)

func TestRender(t *testing.T) {
	t.Parallel()
	changes := []compat.Change{
		{Version: "v0.103.0", Date: "2026-10-18", Class: "breaking", Qualifier: "behavioural", Summary: "a thing changes.", OffSwitch: "`X=off`"},
		{Version: "v0.102.60", Date: "2026-10-17", Class: "breaking", Qualifier: "only for 0.102.59", Summary: "gone."},
		{Version: "v0.102.1", Date: "2026-09-27", Class: "breaking", Summary: "the API changed."},
		{Version: "v0.102.0", Date: "2026-09-26", Class: "behavioural", Qualifier: "patch-safe", Summary: "fewer requests pass.", Reason: "only failing ones."},
		{Version: "v0.101.0", Date: "2026-09-20", Class: "security", Summary: "covered fix."},
		{Version: "v0.106.0", Date: "2026-10-20", Class: "security", Summary: "latent hole closed."},
		{Version: "v0.106.1", Date: "2026-10-21", Class: "security", Summary: "undecided fix."},
	}
	m := compat.Manifest{Schema: 1, MinSafe: "v0.102.2", MinSafeUnchanged: []compat.Unchanged{{Version: "v0.106.0", Reason: "not reachable on a typical host"}}}
	want := "### go-common changes to check between `v0.102.0` and `v0.103.0`\n\n" +
		"The CHANGELOG.md bullets marked BREAKING, BEHAVIOURAL or SECURITY in that range, one line each; the entry has the full text and the reason.\n\n" +
		"- **v0.103.0** (2026-10-18), BREAKING (behavioural): a thing changes.\n" +
		"  Off-switch: `X=off`\n" +
		"- **v0.102.60** (2026-10-17), BREAKING (only for 0.102.59): gone.\n" +
		"- **v0.102.1** (2026-09-27), BREAKING: the API changed.\n" +
		"- **v0.102.0** (2026-09-26), BEHAVIOURAL (patch-safe): fewer requests pass.\n" +
		"  Why a patch release carries it: only failing ones.\n" +
		"- **v0.101.0** (2026-09-20), SECURITY: covered fix.\n" +
		"  Floor: min_safe is v0.102.2, at or above this release, so a pin below it is reported by compatcheck\n" +
		"- **v0.106.0** (2026-10-20), SECURITY: latent hole closed.\n" +
		"  Floor: min_safe is still v0.102.2, on purpose: not reachable on a typical host\n" +
		"- **v0.106.1** (2026-10-21), SECURITY: undecided fix.\n" +
		"  Floor: min_safe is v0.102.2 and no decision is recorded for this release\n"
	if got := render("v0.102.0", "v0.103.0", changes, m); got != want {
		t.Errorf("render with a range:\n%s\nwant:\n%s", got, want)
	}
	if got := render("v0.102.0", "", changes[:1], m); !strings.HasPrefix(got, "### go-common changes to check since `v0.102.0`\n") {
		t.Errorf("render without an upper end: %q", got)
	}
	none := render("v0.105.0", "", nil, m)
	if none != "### go-common changes to check since `v0.105.0`\n\nNone: no CHANGELOG.md entry in that range is marked BREAKING, BEHAVIOURAL or SECURITY.\n" {
		t.Errorf("render of nothing: %q", none)
	}
}

// project writes a module root with the given CHANGELOG.md and, when feed is not nil,
// compat/changes.json.
func project(t *testing.T, changelog string, feed *string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "compat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	if feed != nil {
		if err := os.WriteFile(filepath.Join(root, "compat", "changes.json"), []byte(*feed), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

const goodChangelog = "# Changelog\n\n" +
	"## [0.3.0] - 2026-03-01\n\n- **BREAKING (behavioural):** a\n  - summary: a changes.\n  - off-switch: `A=off`\n\n" +
	"## [0.2.0] - 2026-02-01\n\n- a plain bullet\n"

func TestGenCheckRelease(t *testing.T) {
	t.Parallel()
	root := project(t, goodChangelog, nil)

	if code, _, errOut := runCLI("-check", "-root", root); code != 1 || !strings.Contains(errOut, "compat/changes.json is missing") {
		t.Fatalf("-check with no feed: exit %d, %q", code, errOut)
	}
	if code, out, errOut := runCLI("-gen", "-root", root); code != 0 || !strings.Contains(out, "wrote compat/changes.json (1 changes)") {
		t.Fatalf("-gen: exit %d, %q, %q", code, out, errOut)
	}
	if code, out, errOut := runCLI("-check", "-root", root); code != 0 || !strings.Contains(out, "is current (1 changes)") {
		t.Fatalf("-check after -gen: exit %d, %q, %q", code, out, errOut)
	}
	if code, out, errOut := runCLI("-release", "0.3.0", "-root", root); code != 0 || !strings.Contains(out, "v0.3.0: 1 feed bullets") {
		t.Fatalf("-release: exit %d, %q, %q", code, out, errOut)
	}
	if code, _, errOut := runCLI("-release", "v0.2.0", "-root", root); code != 0 {
		t.Fatalf("-release of an entry with no feed bullets: exit %d, %q", code, errOut)
	}
	if code, _, errOut := runCLI("-release", "0.9.9", "-root", root); code != 1 || !strings.Contains(errOut, "has no entry for v0.9.9") {
		t.Fatalf("-release of a missing entry: exit %d, %q", code, errOut)
	}

	// The generated file is what the library reads.
	data, err := os.ReadFile(filepath.Join(root, "compat", "changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	feed, err := compat.ParseChanges(data)
	if err != nil {
		t.Fatalf("generated feed does not parse: %v", err)
	}
	if len(feed.Changes) != 1 || feed.Changes[0].OffSwitch != "`A=off`" {
		t.Fatalf("feed = %+v", feed)
	}

	// Edit the summary without regenerating: stale, in both modes that look.
	edited := strings.Replace(goodChangelog, "a changes.", "a changes differently.", 1)
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-check", "-root", root}, {"-release", "0.3.0", "-root", root}} {
		if code, _, errOut := runCLI(args...); code != 1 || !strings.Contains(errOut, "is stale") || !strings.Contains(errOut, "go run ./scripts/whatchanged -gen") {
			t.Errorf("%v on a stale feed: exit %d, %q", args, code, errOut)
		}
	}
	// -gen repairs it.
	if code, _, errOut := runCLI("-gen", "-root", root); code != 0 {
		t.Fatalf("-gen: exit %d, %q", code, errOut)
	}
	if code, _, errOut := runCLI("-check", "-root", root); code != 0 {
		t.Fatalf("-check after repair: exit %d, %q", code, errOut)
	}
}

func TestReleaseWithLatestTag(t *testing.T) {
	t.Parallel()
	cl := "# Changelog\n\n## [0.4.1] - 2026-03-01\n\n- **BREAKING (behavioural):** a\n  - summary: a changes.\n\n## [0.4.0] - 2026-02-01\n\n- plain\n"
	root := project(t, cl, nil)
	if code, _, errOut := runCLI("-gen", "-root", root); code != 0 {
		t.Fatalf("-gen: %d %q", code, errOut)
	}
	// Without a tag there is nothing to compare with; after 0.4.0 it is a patch; after 0.3.9 it is a minor.
	for tag, want := range map[string]int{"": 0, "v0.4.0": 1, "0.4.0": 1, "v0.3.9": 0} {
		args := []string{"-release", "0.4.1", "-root", root}
		if tag != "" {
			args = append(args, "-latest-tag", tag)
		}
		code, _, errOut := runCLI(args...)
		if code != want || (want == 1 && !strings.Contains(errOut, "CHANGELOG.md:5:")) {
			t.Errorf("tag %q: exit %d, %q; want %d", tag, code, errOut, want)
		}
	}
}

func TestReleaseSecurityDecision(t *testing.T) {
	t.Parallel()
	cl := "# Changelog\n\n## [0.4.1] - 2026-03-01\n\n- **SECURITY:** a\n  - summary: a fix.\n"
	root := project(t, cl, nil)
	if code, _, errOut := runCLI("-gen", "-root", root); code != 0 {
		t.Fatalf("-gen: %d %q", code, errOut)
	}
	write := func(doc string) {
		if err := os.WriteFile(filepath.Join(root, "compat", "compat.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const base = `{"schema":1,"min_safe":"v0.3.0","min_safe_date":"2026-01-01T00:00:00Z","reasons":[{"version":"v0.3.0","summary":"x"}]%s}`
	for name, tc := range map[string]struct {
		doc  string
		want int
		msg  string
	}{
		"no compat.json":          {"", 1, "cannot read the decision"},
		"no decision":             {strings.Replace(base, "%s", "", 1), 1, "has to settle what the fix does to the floor"},
		"an unchanged record":     {strings.Replace(base, "%s", `,"min_safe_unchanged":[{"version":"v0.4.1","reason":"latent"}]`, 1), 0, ""},
		"a record with no reason": {strings.Replace(base, "%s", `,"min_safe_unchanged":[{"version":"v0.4.1","reason":" "}]`, 1), 1, "has no reason"},
	} {
		if tc.doc == "" {
			_ = os.Remove(filepath.Join(root, "compat", "compat.json"))
		} else {
			write(tc.doc)
		}
		code, _, errOut := runCLI("-release", "0.4.1", "-latest-tag", "v0.4.0", "-root", root)
		if code != tc.want || !strings.Contains(errOut, tc.msg) {
			t.Errorf("%s: exit %d, %q; want %d and %q", name, code, errOut, tc.want, tc.msg)
		}
	}
}

func TestGenRefusesAGrammarProblem(t *testing.T) {
	t.Parallel()
	bad := strings.Replace(goodChangelog, "  - summary: a changes.\n", "", 1)
	root := project(t, bad, nil)
	code, out, errOut := runCLI("-gen", "-root", root)
	if code != 1 || out != "" || !strings.Contains(errOut, "CHANGELOG.md:5: BREAKING (behavioural) of v0.3.0: no summary: line") {
		t.Fatalf("exit %d, %q, %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "compat", "changes.json")); err == nil {
		t.Error("-gen wrote a feed from a CHANGELOG.md it had problems with")
	}
	// A stale feed is not reported on top of grammar problems: fix the grammar first.
	stale := "{}\n"
	root = project(t, bad, &stale)
	if code, _, errOut := runCLI("-check", "-root", root); code != 1 || strings.Contains(errOut, "stale") {
		t.Fatalf("-check: exit %d, %q", code, errOut)
	}
}

func TestPrintMode(t *testing.T) {
	t.Parallel()
	code, out, errOut := runCLI("v0.102.60", "v0.103.0")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	if !strings.Contains(out, "FLEET_EGRESS_DEFAULT_TRANSPORT=off") || !strings.Contains(out, "**v0.103.0** (2026-10-18), BREAKING (behavioural): ") {
		t.Errorf("the 0.103.0 entry, with its off-switch, is missing from:\n%s", out)
	}
	if strings.Contains(out, "**v0.102.60**") {
		t.Errorf("the lower end is exclusive; got:\n%s", out)
	}
	// With and without the v, and with the upper end left out.
	_, a, _ := runCLI("0.102.60", "0.103.0")
	if a != out {
		t.Errorf("0.102.60 0.103.0 differs from v0.102.60 v0.103.0")
	}
	// A release that does not exist yet: the feed has nothing newer. (This used to name the newest
	// release of the day, v0.105.0, and failed as soon as a later release added a BREAKING bullet.)
	if code, out, _ := runCLI("v99.0.0"); code != 0 || !strings.Contains(out, "None:") {
		t.Errorf("nothing is newer than v99.0.0 in the feed: exit %d, %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no arguments":    {},
		"three arguments": {"v0.1.0", "v0.2.0", "v0.3.0"},
		"not a version":   {"banana"},
		"from after to":   {"v0.9.0", "v0.1.0"},
		"two modes":       {"-gen", "-check"},
		"unknown flag":    {"-nope"},
	} {
		if code, _, errOut := runCLI(args...); code != 2 || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q; want 2 and a message", name, code, errOut)
		}
	}
	if code, _, errOut := runCLI("-check", "-root", t.TempDir()); code != 2 || errOut == "" {
		t.Errorf("-check with no CHANGELOG.md: exit %d, %q", code, errOut)
	}
}

// FuzzParseChangelog: whatever CHANGELOG.md holds, parsing does not panic, and what it
// accepts without a problem is a feed that the library's own parser accepts.
func FuzzParseChangelog(f *testing.F) {
	f.Add("")
	f.Add(goodChangelog)
	f.Add(doc("- **BREAKING:** x\n  - summary: a\n"))
	f.Add(doc("- **BREAKING (behavioural): obs is opt-in.** more\n\n  ```go\n  - summary: no\n  ```\n  - summary: yes\n  - off-switch: z\n"))
	f.Add("## v0.1.0 — 2026-01-01\n- **BREAKING ():** x\n  - summary:  y\n  **BREAKING** z\n```\n")
	f.Fuzz(func(t *testing.T, src string) {
		entries, probs := parseChangelog([]byte(src))
		for _, e := range entries {
			if !strings.HasPrefix(e.version, "v") {
				t.Fatalf("version %q", e.version)
			}
		}
		if len(probs) > 0 {
			return
		}
		feed := feedOf(entries)
		data, err := marshalFeed(feed)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := compat.ParseChanges(data)
		if err != nil {
			t.Fatalf("an accepted CHANGELOG.md produced a feed compat rejects: %v\n%s", err, data)
		}
		if len(parsed.Changes) != len(feed.Changes) {
			t.Fatalf("lost changes: %d != %d", len(parsed.Changes), len(feed.Changes))
		}
		for _, c := range parsed.Changes {
			if c.Summary == "" || len([]rune(c.Summary)) > maxFieldLen || len([]rune(c.OffSwitch)) > maxFieldLen {
				t.Fatalf("a change outside the limits was accepted: %+v", c)
			}
		}
	})
}

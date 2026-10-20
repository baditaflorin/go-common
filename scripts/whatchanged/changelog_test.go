package main

import (
	"strings"
	"testing"
)

// doc wraps bullets in one entry, so a case only has to write the part it is about.
func doc(body string) string {
	return "# Changelog\n\n## [0.200.0] - 2026-11-01\n\n### Changed\n\n" + body
}

func mustParse(t *testing.T, src string) []*entry {
	t.Helper()
	entries, probs := parseChangelog([]byte(src))
	if len(probs) > 0 {
		t.Fatalf("unexpected problems: %v", probs)
	}
	return entries
}

// problemsOf returns the messages parseChangelog reports for src, with their lines.
func problemsOf(src string) []string {
	_, probs := parseChangelog([]byte(src))
	var out []string
	for _, p := range probs {
		out = append(out, p.String())
	}
	return out
}

func wantProblem(t *testing.T, name, src, contains string) {
	t.Helper()
	probs := problemsOf(src)
	for _, p := range probs {
		if strings.Contains(p, contains) {
			return
		}
	}
	t.Errorf("%s: no problem mentions %q; got %q", name, contains, probs)
}

func TestHeadingForms(t *testing.T) {
	t.Parallel()
	src := "# Changelog\n\n" +
		"## [0.105.0] - 2026-10-19\n\n- **BREAKING:** a\n  - summary: one\n\n" +
		"## v0.99.1 - 2026-09-20\n\n- **BREAKING:** b\n  - summary: two\n\n" +
		"## v0.97.0 — 2026-09-02\n\n- **BREAKING:** c\n  - summary: three\n"
	entries := mustParse(t, src)
	var got []string
	for _, e := range entries {
		got = append(got, e.version+" "+e.date+" "+e.bullets[0].summary)
	}
	want := []string{"v0.105.0 2026-10-19 one", "v0.99.1 2026-09-20 two", "v0.97.0 2026-09-02 three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

func TestBulletShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body                                   string
		class, qualifier, summary, offSwitch, reason string
	}{
		{
			name:  "behavioural with both fields",
			body:  "- **BREAKING (behavioural):** `x` changes.\n  more prose\n  - summary: `x` changes.\n  - off-switch: `X=off`\n",
			class: "breaking", qualifier: "behavioural", summary: "`x` changes.", offSwitch: "`X=off`",
		},
		{
			name:  "no qualifier, no off-switch",
			body:  "- **BREAKING:** the API changed.\n  - summary: the API changed.\n",
			class: "breaking", summary: "the API changed.",
		},
		{
			name:  "a qualifier that is not behavioural",
			body:  "- **BREAKING (only for 0.102.59):** gone.\n  - summary: gone.\n",
			class: "breaking", qualifier: "only for 0.102.59", summary: "gone.",
		},
		{
			name:  "older entries keep the first sentence in bold, with the colon inside",
			body:  "- **BREAKING (behavioural): `obs` is now opt-in.** v0.61.0 started it.\n  - summary: opt-in now.\n",
			class: "breaking", qualifier: "behavioural", summary: "opt-in now.",
		},
		{
			name:  "blank lines and a code fence inside the bullet; field-looking lines in the fence are not fields",
			body:  "- **BREAKING:** text\n\n  ```go\n  - summary: not a field\n  - off-switch: not one either\n  ```\n\n  more prose\n  - summary: the real one\n",
			class: "breaking", summary: "the real one",
		},
		{
			name:  "fields may sit anywhere inside the bullet, in either order",
			body:  "- **BREAKING:** text\n  - off-switch: first\n  more prose\n  - summary: second\n",
			class: "breaking", summary: "second", offSwitch: "first",
		},
		{
			name:  "a patch-safe behaviour change carries its reason",
			body:  "- **BEHAVIOURAL (patch-safe):** only a request that failed before is refused now.\n  - summary: requests that failed before are refused.\n  - reason: no accepted request changes.\n",
			class: "behavioural", qualifier: "patch-safe", summary: "requests that failed before are refused.", reason: "no accepted request changes.",
		},
		{
			name:  "a bare marker line",
			body:  "- **BREAKING:**\n  - summary: only the marker opened it\n",
			class: "breaking", summary: "only the marker opened it",
		},
	} {
		entries := mustParse(t, doc(tc.body))
		if len(entries) != 1 || len(entries[0].bullets) != 1 {
			t.Errorf("%s: got %d entries", tc.name, len(entries))
			continue
		}
		b := entries[0].bullets[0]
		if b.class != tc.class || b.qualifier != tc.qualifier || b.summary != tc.summary || b.offSwitch != tc.offSwitch || b.reason != tc.reason {
			t.Errorf("%s: got class=%q qualifier=%q summary=%q off-switch=%q reason=%q", tc.name, b.class, b.qualifier, b.summary, b.offSwitch, b.reason)
		}
	}
}

func TestOnlyFeedBulletsAreKept(t *testing.T) {
	t.Parallel()
	src := doc("- an ordinary bullet that says **BREAKING:** in the middle\n" +
		"- **Fixed:** something\n" +
		"- **BREAKING:** the one\n  - summary: kept\n" +
		"- another ordinary bullet\n\n" +
		"```text\n- **BREAKING:** inside a fence\n```\n")
	entries := mustParse(t, src)
	if n := len(entries[0].bullets); n != 1 || entries[0].bullets[0].summary != "kept" {
		t.Fatalf("kept %d bullets", n)
	}
}

func TestFieldsBelongToTheirOwnBullet(t *testing.T) {
	t.Parallel()
	// The first bullet ends at the next bullet, so the summary that follows is the second one's.
	src := doc("- **BREAKING:** first\n- **BREAKING:** second\n  - summary: belongs to the second\n")
	probs := problemsOf(src)
	if len(probs) != 1 || !strings.Contains(probs[0], "no summary: line") || !strings.Contains(probs[0], ":7:") {
		t.Fatalf("problems = %q, want one, at the first bullet's line", probs)
	}
	// A paragraph in column 0 also ends it.
	wantProblem(t, "paragraph ends the bullet", doc("- **BREAKING:** first\n\nA paragraph.\n  - summary: too late\n"), "no summary: line")
	wantProblem(t, "heading ends the bullet", doc("- **BREAKING:** first\n\n### Added\n  - summary: too late\n"), "no summary: line")
}

func TestFieldLengthCountsCharacters(t *testing.T) {
	t.Parallel()
	ascii200 := strings.Repeat("a", 200)
	accent200 := strings.Repeat("é", 200) // 400 bytes
	for _, tc := range []struct {
		name, line string
		ok         bool
	}{
		{"200 characters", "  - summary: " + ascii200, true},
		{"201 characters", "  - summary: " + ascii200 + "a", false},
		{"200 multi-byte characters", "  - summary: " + accent200, true},
		{"201 multi-byte characters", "  - summary: " + accent200 + "é", false},
	} {
		probs := problemsOf(doc("- **BREAKING:** x\n" + tc.line + "\n"))
		if tc.ok && len(probs) > 0 {
			t.Errorf("%s: %q", tc.name, probs)
		}
		if !tc.ok && (len(probs) != 1 || !strings.Contains(probs[0], "the limit is 200")) {
			t.Errorf("%s: want one length problem, got %q", tc.name, probs)
		}
	}
	wantProblem(t, "off-switch limit", doc("- **BREAKING:** x\n  - summary: s\n  - off-switch: "+ascii200+"a\n"), "off-switch: is 201 characters")
}

func TestFieldProblems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"missing summary", "- **BREAKING:** x\n  - off-switch: o\n", "no summary: line"},
		{"duplicate summary", "- **BREAKING:** x\n  - summary: a\n  - summary: b\n", "summary: is given twice"},
		{"duplicate off-switch", "- **BREAKING:** x\n  - summary: a\n  - off-switch: a\n  - off-switch: b\n", "off-switch: is given twice"},
		{"three spaces", "- **BREAKING:** x\n   - summary: a\n", "looks like a field"},
		{"a tab", "- **BREAKING:** x\n\t- summary: a\n", "looks like a field"},
		{"no dash", "- **BREAKING:** x\n  summary: a\n", "looks like a field"},
		{"capitalised", "- **BREAKING:** x\n  - Summary: a\n", "looks like a field"},
		{"two spaces after the colon", "- **BREAKING:** x\n  - summary:  a\n", "looks like a field"},
		{"trailing space", "- **BREAKING:** x\n  - summary: a \n", "looks like a field"},
		{"empty value", "- **BREAKING:** x\n  - summary: \n", "looks like a field"},
		{"no space after the colon", "- **BREAKING:** x\n  - summary:a\n", "looks like a field"},
		{"a field at column 0 is not inside the bullet", "- **BREAKING:** x\nsummary: a\n", "no summary: line"},
	} {
		wantProblem(t, tc.name, doc(tc.body), tc.want)
	}
}

func TestMarkerProblems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"colon outside the bold", "- **BREAKING**: x\n  - summary: a\n", "bullet but not as"},
		{"a longer marker word", "- **BREAKING CHANGE:** x\n  - summary: a\n", "bullet but not as"},
		{"text glued to the colon", "- **BREAKING:**x\n  - summary: a\n", ""},
		{"empty qualifier", "- **BREAKING ():** x\n  - summary: a\n", "empty qualifier"},
		{"nested parentheses", "- **BREAKING (a (b)):** x\n  - summary: a\n", "bullet but not as"},
		{"BEHAVIOURAL without its qualifier", "- **BEHAVIOURAL:** x\n  - summary: a\n  - reason: r\n", "BEHAVIOURAL has one form"},
		{"BEHAVIOURAL with another qualifier", "- **BEHAVIOURAL (urgent):** x\n  - summary: a\n  - reason: r\n", "BEHAVIOURAL has one form"},
		{"BEHAVIOURAL without a reason", "- **BEHAVIOURAL (patch-safe):** x\n  - summary: a\n", "no reason: line"},
		{"BEHAVIOURAL without a summary", "- **BEHAVIOURAL (patch-safe):** x\n  - reason: r\n", "no summary: line"},
		{"a reason on a BREAKING bullet", "- **BREAKING (behavioural):** x\n  - summary: a\n  - reason: r\n", "reason: belongs to a BEHAVIOURAL"},
		{"BEHAVIOURAL colon outside the bold", "- **BEHAVIOURAL (patch-safe)**: x\n  - summary: a\n  - reason: r\n", "bullet but not as"},
		{"BEHAVIOURAL on a wrapped line", "- an ordinary bullet that\n  **BEHAVIOURAL (patch-safe):** carries on\n", "has to open a top-level"},
		{"another bullet character", "* **BREAKING:** x\n  - summary: a\n", "has to open a top-level"},
		{"a paragraph", "**BREAKING:** x\n", "has to open a top-level"},
		{"a nested bullet", "- an ordinary bullet\n  - **BREAKING:** nested\n", "has to open a top-level"},
		{"a wrapped line", "- an ordinary bullet that\n  **BREAKING** carries on\n", "has to open a top-level"},
	} {
		if tc.want == "" {
			// "**BREAKING:**x" is a valid marker: the closing ** follows the colon.
			mustParse(t, doc(tc.body))
			continue
		}
		wantProblem(t, tc.name, doc(tc.body), tc.want)
	}
	// What apidiff-check.sh accepts as a declaration, this refuses unless it is well formed:
	// a looser reading would let a bullet fall out of the feed.
	for _, line := range []string{"  **BREAKING:** x", "- **BREAKING (behavioural):** x", "- **BREAKING:** x", "**BREAKING** x"} {
		if !looseMarkerRe.MatchString(line) {
			t.Errorf("looseMarkerRe misses %q", line)
		}
	}
}

func TestStructureProblems(t *testing.T) {
	t.Parallel()
	wantProblem(t, "bullet before any entry", "# Changelog\n\n- **BREAKING:** x\n  - summary: a\n", "outside an entry")
	wantProblem(t, "heading form", "# Changelog\n\n## Unreleased\n", "is not")
	wantProblem(t, "heading without a date", "# Changelog\n\n## [0.1.0]\n", "is not")
	wantProblem(t, "impossible date", "# Changelog\n\n## [0.1.0] - 2026-13-45\n", "not a calendar date")
	wantProblem(t, "duplicate version", "# Changelog\n\n## [0.1.0] - 2026-01-01\n\n## v0.1.0 - 2026-01-02\n", "already has an entry, at line 3")
	wantProblem(t, "unclosed fence", doc("- **BREAKING:** x\n  - summary: a\n\n  ```go\n  code\n"), "never closed")
	// A bullet after a bad heading is not attributed to the entry above it.
	probs := problemsOf("# Changelog\n\n## [0.2.0] - 2026-01-02\n\n## Unreleased\n\n- **BREAKING:** x\n  - summary: a\n")
	if len(probs) != 2 {
		t.Errorf("problems = %q, want the heading and the orphaned bullet", probs)
	}
}

func TestFenceHandling(t *testing.T) {
	t.Parallel()
	// A heading, a marker and a field inside a fence are text.
	src := doc("- **BREAKING:** x\n  ```markdown\n  ## [9.9.9] - 2026-01-01\n  - **BREAKING:** not a bullet\n  ```\n  - summary: ok\n\n~~~\n## not a heading\n~~~\n")
	entries := mustParse(t, src)
	if len(entries) != 1 || len(entries[0].bullets) != 1 {
		t.Fatalf("entries = %d, bullets = %d", len(entries), len(entries[0].bullets))
	}
	// Inline code at the start of a line is not a fence.
	mustParse(t, doc("- **BREAKING:** x\n  ```inline``` text\n  - summary: ok\n"))
	// A fence in column 0 ends the bullet, so a field after it is not the bullet's.
	wantProblem(t, "column-0 fence ends the bullet", doc("- **BREAKING:** x\n\n```\ncode\n```\n  - summary: too late\n"), "no summary: line")
	// A longer closing fence closes; a shorter one does not.
	wantProblem(t, "short closing fence", doc("- **BREAKING:** x\n  - summary: a\n\n````\ncode\n```\n"), "never closed")
	mustParse(t, doc("- **BREAKING:** x\n  - summary: a\n\n```\ncode\n`````\n"))
}

func TestCRLFIsTolerated(t *testing.T) {
	t.Parallel()
	src := strings.ReplaceAll(doc("- **BREAKING:** x\n  - summary: a\n  - off-switch: b\n"), "\n", "\r\n")
	entries := mustParse(t, src)
	if b := entries[0].bullets[0]; b.summary != "a" || b.offSwitch != "b" {
		t.Fatalf("got %+v", b)
	}
}

func TestFeedOfKeepsFileOrder(t *testing.T) {
	t.Parallel()
	src := "# Changelog\n\n" +
		"## [0.3.0] - 2026-03-01\n\n- **BREAKING (behavioural):** a\n  - summary: a\n  - off-switch: oa\n- **BREAKING:** b\n  - summary: b\n\n" +
		"## [0.2.0] - 2026-02-01\n\n- nothing here\n\n" +
		"## v0.1.0 - 2026-01-01\n\n- **BREAKING (only for 0.0.9):** c\n  - summary: c\n"
	f := feedOf(mustParse(t, src))
	var got []string
	for _, c := range f.Changes {
		got = append(got, c.Version+"/"+c.Qualifier+"/"+c.Summary+"/"+c.OffSwitch)
	}
	want := "v0.3.0/behavioural/a/oa|v0.3.0//b/|v0.1.0/only for 0.0.9/c/"
	if strings.Join(got, "|") != want {
		t.Fatalf("changes = %q", got)
	}
	if f.Schema != 1 {
		t.Errorf("schema = %d", f.Schema)
	}
	if empty := feedOf(nil); empty.Changes == nil {
		t.Error("an empty feed must hold [] and not null")
	}
}

func TestPatchBump(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version, tag string
		want         bool
	}{
		{"0.105.1", "v0.105.0", true},
		{"0.105.9", "v0.105.0", true},
		{"0.106.0", "v0.105.0", false},
		{"0.105.1", "v0.104.7", false},
		{"1.0.0", "v0.105.0", false},
		{"0.105.1", "", false},
		{"0.105.1", "garbage", false},
		{"0.105.1", "v0.105.0-rc.1", true},
	} {
		if got := patchBump(tc.version, tc.tag); got != tc.want {
			t.Errorf("patchBump(%q, %q) = %v, want %v", tc.version, tc.tag, got, tc.want)
		}
	}
}

func TestReleaseProblems(t *testing.T) {
	t.Parallel()
	body := "- **BREAKING (behavioural):** a\n  - summary: a\n" +
		"- **BREAKING:** the API\n  - summary: b\n" +
		"- **BREAKING (only for 0.1.0):** c\n  - summary: c\n" +
		"- **BEHAVIOURAL (patch-safe):** d\n  - summary: d\n  - reason: r\n"
	src := "# Changelog\n\n## [%s] - 2026-11-01\n\n" + body
	for _, tc := range []struct {
		version, tag string
		want         int // problems
	}{
		{"0.105.1", "v0.105.0", 1}, // a patch with a BREAKING (behavioural) bullet
		{"0.106.0", "v0.105.0", 0}, // a minor
		{"1.0.0", "v0.105.0", 0},
		{"0.105.1", "", 0}, // no tag, nothing to compare with
	} {
		entries := mustParse(t, strings.Replace(src, "%s", tc.version, 1))
		probs := releaseProblems(entries[0], tc.tag)
		if len(probs) != tc.want {
			t.Errorf("%s after %q: %d problems %v, want %d", tc.version, tc.tag, len(probs), probs, tc.want)
		}
		for _, p := range probs {
			if p.line != 5 || !strings.Contains(p.msg, "takes a minor release") || !strings.Contains(p.msg, "BEHAVIOURAL (patch-safe)") {
				t.Errorf("problem %v: want the first bullet (line 5) and the way out", p)
			}
		}
	}
}

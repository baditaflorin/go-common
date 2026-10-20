package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/go-common/compat"
)

// The grammar of the bullets the feed carries (docs/releasing.md has the prose):
//
//	entry   = "## [X.Y.Z] - YYYY-MM-DD" | "## vX.Y.Z - YYYY-MM-DD" | "## vX.Y.Z — YYYY-MM-DD"
//	bullet  = "- **" CLASS [ " (" qualifier ")" ] ":" ... , at column 0, inside an entry
//	CLASS   = "BREAKING" | "BEHAVIOURAL"   (BEHAVIOURAL takes the qualifier "patch-safe", and only that)
//	field   = "  - summary: " text | "  - off-switch: " text | "  - reason: " text   (two spaces, inside the bullet)
//
// A bullet runs from its "- " line to the line before the next line that starts in
// column 0 (the next bullet, a heading, a paragraph); blank lines and indented lines
// belong to it. Fenced code is skipped.
const (
	// maxFieldLen is the limit, in characters, of a summary: or off-switch: value.
	maxFieldLen    = 200
	fieldSummary   = "summary"
	fieldOffSwitch = "off-switch"
	fieldReason    = "reason"

	// patchSafe is the one qualifier of a BEHAVIOURAL bullet.
	patchSafe = "patch-safe"
)

var (
	// headingRe is every form "## " takes in CHANGELOG.md.
	headingRe = regexp.MustCompile(`^## (?:\[(\d+\.\d+\.\d+)\]|v(\d+\.\d+\.\d+)) [-—] (\d{4}-\d{2}-\d{2})$`)
	// markerRe opens a feed bullet. The colon is inside the bold text; what follows it
	// is either the closing ** or, in older entries, the first sentence in bold.
	markerRe = regexp.MustCompile(`^- \*\*(BREAKING|BEHAVIOURAL)(?: \(([^()]*)\))?:(?:\*\*| |$)`)
	// looseMarkerRe is anything that reads as an attempt to write a marker, on any line:
	// a superset of what scripts/apidiff-check.sh accepts as "declared BREAKING". A line
	// that matches it but is not a well-formed marker at the start of a top-level bullet
	// is an error, so a bullet cannot be left out of the feed by a typo.
	looseMarkerRe = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\*\*(?:BREAKING|BEHAVIOURAL)`)
	// fieldRe is a field line: exactly two spaces, "- ", the name, ": " and a value
	// that has no space at either end.
	fieldRe = regexp.MustCompile(`^  - (summary|off-switch|reason): (\S(?:.*\S)?)$`)
	// nearFieldRe finds a line that is meant to be a field but is not written like one.
	nearFieldRe = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s+)?(summary|off-switch|reason)\s*:`)
)

// problem is something in CHANGELOG.md the feed cannot accept.
type problem struct {
	line int
	msg  string
}

// String is "CHANGELOG.md:LINE: message", or only the message for a problem that is not about a line.
func (p problem) String() string {
	if p.line == 0 {
		return p.msg
	}
	return fmt.Sprintf("%s:%d: %s", changelogPath, p.line, p.msg)
}

// entry is one release section of CHANGELOG.md.
type entry struct {
	version string // "v0.105.0"
	date    string // "2026-10-19"
	line    int
	bullets []*bullet // feed bullets only, in file order
}

// bullet is one feed bullet: its marker, and the fields appended to it.
type bullet struct {
	line      int
	class     string // "breaking"
	qualifier string
	summary   string
	offSwitch string
	reason    string
	lines     []bulletLine
}

type bulletLine struct {
	n      int
	text   string
	fenced bool
}

// marker is the bullet's marker as the author wrote it, for messages.
func (b *bullet) marker() string {
	m := strings.ToUpper(b.class)
	if b.qualifier != "" {
		m += " (" + b.qualifier + ")"
	}
	return m
}

// parseChangelog reads CHANGELOG.md and returns its entries, in file order, with the
// feed bullets of each. The problems are everything the feed could not take; when there
// are any, the entries are not to be trusted.
func parseChangelog(src []byte) ([]*entry, []problem) {
	var (
		entries   []*entry
		probs     []problem
		cur       *entry
		open      *bullet
		fenceCh   byte
		fenceLen  int
		fenceLine int
		seen      = map[string]int{}
	)
	bad := func(n int, format string, a ...any) { probs = append(probs, problem{n, fmt.Sprintf(format, a...)}) }
	finish := func() {
		if open != nil {
			probs = append(probs, checkBullet(cur, open)...)
			open = nil
		}
	}

	for i, line := range strings.Split(string(src), "\n") {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " ")

		if fenceCh != 0 {
			if ch, l := fenceOf(trimmed); ch == fenceCh && l >= fenceLen && strings.TrimSpace(trimmed[l:]) == "" {
				fenceCh = 0
			}
			if open != nil {
				open.lines = append(open.lines, bulletLine{n, line, true})
			}
			continue
		}
		if ch, l := fenceOf(trimmed); ch != 0 {
			if line[0] != ' ' && line[0] != '\t' {
				finish() // a fence in column 0 is not part of a list item
			}
			fenceCh, fenceLen, fenceLine = ch, l, n
			if open != nil {
				open.lines = append(open.lines, bulletLine{n, line, true})
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "## "):
			finish()
			m := headingRe.FindStringSubmatch(line)
			if m == nil {
				bad(n, "heading %q is not \"## [X.Y.Z] - YYYY-MM-DD\" (or \"## vX.Y.Z - YYYY-MM-DD\")", line)
				cur = nil
				continue
			}
			v := "v" + m[1] + m[2] // exactly one of the two groups is set
			if _, err := time.Parse(time.DateOnly, m[3]); err != nil {
				bad(n, "heading date %q is not a calendar date", m[3])
			}
			if prev, dup := seen[v]; dup {
				bad(n, "%s already has an entry, at line %d", v, prev)
			}
			seen[v] = n
			cur = &entry{version: v, date: m[3], line: n}
			entries = append(entries, cur)

		case strings.HasPrefix(line, "- "):
			finish()
			m := markerRe.FindStringSubmatchIndex(line)
			switch {
			case m != nil:
				if cur == nil {
					bad(n, "%s bullet outside an entry", line[m[2]:m[3]])
					continue
				}
				b := &bullet{line: n, class: strings.ToLower(line[m[2]:m[3]])}
				if m[4] >= 0 { // a qualifier in parentheses
					if b.qualifier = strings.TrimSpace(line[m[4]:m[5]]); b.qualifier == "" {
						bad(n, "%s has an empty qualifier \"()\"; leave the parentheses out or say what it is", strings.ToUpper(b.class))
					}
				}
				b.lines = []bulletLine{{n, line, false}}
				cur.bullets = append(cur.bullets, b)
				open = b
			case looseMarkerRe.MatchString(line):
				bad(n, "starts a BREAKING or BEHAVIOURAL bullet but not as \"- **BREAKING:** ...\", \"- **BREAKING (qualifier):** ...\" or \"- **BEHAVIOURAL (patch-safe):** ...\"; the feed cannot carry it")
			}

		case line != "" && line[0] != ' ' && line[0] != '\t':
			// A heading, a paragraph or a list in another style, in column 0: the bullet is over.
			finish()
			if looseMarkerRe.MatchString(line) {
				bad(n, "a BREAKING or BEHAVIOURAL marker has to open a top-level \"- \" bullet; this line is not one")
			}

		default: // blank or indented: inside the bullet if there is one
			if open != nil {
				open.lines = append(open.lines, bulletLine{n, line, false})
			}
			if looseMarkerRe.MatchString(line) {
				bad(n, "a BREAKING or BEHAVIOURAL marker has to open a top-level \"- \" bullet; a nested or wrapped line cannot carry one")
			}
		}
	}
	if fenceCh != 0 {
		bad(fenceLine, "code fence is never closed")
	}
	finish()
	return entries, probs
}

// fenceOf reports the character and length of a code fence that starts the text, or 0.
func fenceOf(trimmed string) (byte, int) {
	if trimmed == "" || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	if n < 3 {
		return 0, 0
	}
	if trimmed[0] == '`' && strings.Contains(trimmed[n:], "`") {
		return 0, 0 // inline code that starts a line, not a fence
	}
	return trimmed[0], n
}

// patchBump reports whether claiming version (X.Y.Z, no v) is a patch release: the same
// major and minor as the newest tag.
func patchBump(version, latestTag string) bool {
	vm := versionCoreRe.FindStringSubmatch("v" + version)
	tm := versionCoreRe.FindStringSubmatch(latestTag)
	return vm != nil && tm != nil && vm[1] == tm[1] && vm[2] == tm[2]
}

var versionCoreRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)

// releaseProblems are the rules for a change that claims entry e as its release, on top
// of the grammar: a behaviour change takes a minor release, unless its bullet says it is
// safe in a patch and why. latestTag is the newest tag, "" when there is none.
func releaseProblems(e *entry, latestTag string) []problem {
	var probs []problem
	if latestTag == "" || !patchBump(strings.TrimPrefix(e.version, "v"), latestTag) {
		return nil
	}
	for _, b := range e.bullets {
		if b.class == "breaking" && b.qualifier == "behavioural" {
			probs = append(probs, problem{b.line, fmt.Sprintf(
				"%s of %s: a behaviour change takes a minor release, and %s follows %s in the same minor: claim the next minor, or, if a service can take it in a patch, mark the bullet \"**BEHAVIOURAL (patch-safe):**\" and give it a \"  - reason: ...\" line saying why (docs/api-policy.md)",
				b.marker(), e.version, e.version, latestTag)})
		}
	}
	return probs
}

// checkBullet reads the field lines of a closed feed bullet and says what is wrong
// with it. A bullet that passes has its summary and off-switch set.
func checkBullet(e *entry, b *bullet) []problem {
	var probs []problem
	where := fmt.Sprintf("%s of %s", b.marker(), e.version)
	bad := func(n int, format string, a ...any) {
		probs = append(probs, problem{n, where + ": " + fmt.Sprintf(format, a...)})
	}

	have := map[string]int{}
	for _, l := range b.lines[1:] {
		if l.fenced {
			continue
		}
		m := fieldRe.FindStringSubmatch(l.text)
		if m == nil {
			if nearFieldRe.MatchString(l.text) {
				bad(l.n, "line looks like a field but is not exactly \"  - summary: <text>\" or \"  - off-switch: <text>\" (two spaces, a dash, one space after the colon, no trailing space)")
			}
			continue
		}
		name, value := m[1], m[2]
		if first, dup := have[name]; dup {
			bad(l.n, "%s: is given twice (first at line %d)", name, first)
			continue
		}
		have[name] = l.n
		if c := utf8.RuneCountInString(value); c > maxFieldLen {
			bad(l.n, "%s: is %d characters; the limit is %d", name, c, maxFieldLen)
		}
		switch name {
		case fieldSummary:
			b.summary = value
		case fieldOffSwitch:
			b.offSwitch = value
		case fieldReason:
			b.reason = value
		}
	}
	if _, ok := have[fieldSummary]; !ok {
		bad(b.line, "no summary: line; end the bullet with \"  - summary: <one line, at most %d characters: what changes for a service that bumps, and what to check>\" (docs/releasing.md)", maxFieldLen)
	}
	switch b.class {
	case "behavioural":
		if b.qualifier != patchSafe {
			bad(b.line, "BEHAVIOURAL has one form, \"- **BEHAVIOURAL (patch-safe):** ...\"; a behaviour change that is not safe in a patch is \"- **BREAKING (behavioural):** ...\"")
		}
		if _, ok := have[fieldReason]; !ok {
			bad(b.line, "no reason: line; say why a patch release may carry it: \"  - reason: <one line, at most %d characters>\" (docs/api-policy.md)", maxFieldLen)
		}
	default:
		if n, ok := have[fieldReason]; ok {
			bad(n, "reason: belongs to a BEHAVIOURAL (patch-safe) bullet only")
		}
	}
	return probs
}

// feedOf is the feed a parsed CHANGELOG.md says: one change for each feed bullet, in file order.
func feedOf(entries []*entry) compat.ChangeFeed {
	f := compat.ChangeFeed{
		Schema:  1,
		Note:    "Generated from CHANGELOG.md by `go run ./scripts/whatchanged -gen`; do not edit by hand. The grammar is in docs/releasing.md.",
		Changes: []compat.Change{},
	}
	for _, e := range entries {
		for _, b := range e.bullets {
			f.Changes = append(f.Changes, compat.Change{
				Version:   e.version,
				Date:      e.date,
				Class:     b.class,
				Qualifier: b.qualifier,
				Summary:   b.summary,
				OffSwitch: b.offSwitch,
				Reason:    b.reason,
			})
		}
	}
	return f
}

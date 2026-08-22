// Package wordbound is the canonical fix for a Go regexp footgun that has
// independently bitten at least two fleet services: `\b` (word boundary) is
// ASCII-only — per the regexp/syntax docs, a "word" character for `\b`
// purposes is exactly [0-9A-Za-z_]. Any letter outside that set (Cyrillic,
// Han, Hangul, Greek, or an accented/extended Latin letter like "ē"/"ş"/"Ü")
// is treated as NON-word by `\b`, regardless of how a human reader would
// parse the text. That mismatch causes two distinct, opposite-looking bugs
// from the same root cause:
//
//  1. False negative (dead code): a pattern ending in `\bOÜ\b` or `\bΑΕ\b`
//     can never match, because `\b` can't assert a boundary next to the
//     non-ASCII suffix letter itself. go_gdpr_compliance's controller_identity.go
//     shipped a completely dead "OÜ" (Estonian) alternative for an unknown
//     number of releases before a real Estonian fixture (round 27) exposed
//     it — the working fix was a same-shape regex with `Ü` NOT immediately
//     preceded/followed by `\b`.
//
//  2. False positive (silent over-match / truncation): a pattern like
//     `\bSL\b` matching case-insensitively inside "Slēgts" (Latvian for
//     "Closed") — `\b` incorrectly asserts a boundary right after "Sl"
//     because the very next character, "ē", isn't an ASCII word char, even
//     though "Slēgts" is one continuous real word. Confirmed directly:
//     go_gdpr_compliance round 26 found `legalFormSuffixRE.MatchString("S.-Sv.
//     Slēgts")` was true — a real Latvian business-hours line ("Sat-Sun:
//     Closed"), not a company name, scored as a legal-entity-name match.
//     The mirror-image variant is a LEADING `\b` right before a `\p{L}`
//     group: go_update_frequency's `monthNameMDYRE` used `\b([\p{L}]{3,})`
//     to capture a month name, and on Turkish "Şubat" (February) `\b` can't
//     assert a boundary before "Ş", so the regex engine silently slides the
//     match start forward and captures "ubat" instead of "Şubat" — a
//     confirmed, reachable bug fixed alongside this package landing.
//
// go_legal_entity independently discovered and fixed this exact class years
// before either regexp-based case above: its containsSuffix/isWordRune/
// prevRune trio decodes actual runes and checks unicode.IsLetter/IsDigit
// instead of trusting `\b`. go_tos_finder vendored a byte-for-byte copy of
// that trio (see its own file-level comment: "a future pass... should port
// it from go_legal_entity rather than reinventing it") rather than importing
// it, because no shared home existed. This package IS that shared home —
// the single source of truth every fleet service should reach for instead
// of re-deriving (or mis-deriving, via `\b`) the same fix a third time.
//
// Two consumption styles are supported, matching the two ways services
// already do suffix/token matching in this fleet:
//
//   - Manual scanning (as go_legal_entity/go_tos_finder do): call
//     ContainsToken directly, or compose IsWordRune/PrevRune/IsGluedScript
//     for a custom scan.
//   - Regex composition (as go_gdpr_compliance/go_update_frequency do):
//     splice the LeadingBoundary/TrailingBoundary string constants into a
//     hand-built pattern in place of a bare `\b`.
package wordbound

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// LeadingBoundary is a Unicode-safe replacement for a leading `\b` in a
// regexp pattern that is immediately followed by a `\p{L}`-based character
// class. Splice it in literally, e.g.:
//
//	regexp.MustCompile(wordbound.LeadingBoundary + `([\p{L}]{3,})\.?\s+...`)
//
// Unlike `\b`, it correctly requires "start of string, or a non-letter",
// so it won't falsely fail to anchor before a non-ASCII letter (the
// go_update_frequency "Şubat" truncation bug this constant fixes).
const LeadingBoundary = `(?:^|[^\p{L}])`

// TrailingBoundary is a Unicode-safe replacement for a trailing `\b` in a
// regexp pattern that immediately follows a `\p{L}`-based character class or
// a short literal alternation meant to end a word. Splice it in literally,
// e.g.:
//
//	regexp.MustCompile(`...(?:SL|AG|SE)` + wordbound.TrailingBoundary)
//
// Unlike `\b`, it correctly requires "a non-letter, or end of string", so it
// won't falsely succeed right after a short match that's actually a prefix
// of a longer real word in another script (the go_gdpr_compliance "Slēgts"
// false-positive this constant fixes).
const TrailingBoundary = `(?:[^\p{L}]|$)`

// IsWordRune reports whether r is a letter or digit in ANY script — the
// Unicode-aware generalisation of what Go's regexp `\b` treats as a "word"
// character (which is ASCII-only: [0-9A-Za-z_]). Use this instead of `\b`
// semantics when manually scanning text that may contain non-Latin scripts
// or accented Latin letters.
func IsWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// PrevRune decodes the rune ending immediately before byte offset pos in s.
// Returns utf8.RuneError if pos is 0 or s[:pos] ends mid-sequence.
func PrevRune(s string, pos int) rune {
	r, _ := utf8.DecodeLastRuneInString(s[:pos])
	return r
}

// IsGluedScript reports whether token is written in a space-free script
// (Han / Hangul / Hiragana / Katakana) where a preceding name conventionally
// runs directly into the token with no separator — e.g. "株式会社ソニー",
// "삼성전자주식회사". Word-boundary scans should NOT reject a preceding
// letter as disqualifying for tokens in these scripts, unlike Latin/Cyrillic/
// Greek tokens where a preceding letter usually means the match is inside a
// longer word.
func IsGluedScript(token string) bool {
	for _, r := range token {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
			unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return true
		}
	}
	return false
}

// ContainsToken reports whether text contains token as a standalone token
// (loose word boundary) — Unicode-safe, so it correctly rejects e.g. "Inc"
// matching inside "Income" or Cyrillic "АО" matching inside "ПАО"/"ОАО", AND
// correctly rejects the "SL" inside Latvian "Slēgts" false-positive `\b`
// misses. Also rejects token occurrences immediately preceded by '(' — that
// shape is almost always a parenthetical aside ("Equity crowdfunding
// (Limited availability)"), not a genuine suffix/token match.
//
// Boundary detection: a multibyte letter or digit (any script) immediately
// adjacent to the match is treated as a continuing word character, EXCEPT
// for glued scripts (Han/Hangul/Hiragana/Katakana, per IsGluedScript(token)),
// where a preceding letter is allowed since the name legitimately runs
// directly into the token with no separator.
//
// This is the same algorithm go_legal_entity's containsSuffix and
// go_tos_finder's vendored copy already used — promoted here so both (and
// any future consumer) share one implementation instead of three.
func ContainsToken(text, token string) bool {
	glued := IsGluedScript(token)
	idx := 0
	for {
		j := strings.Index(text[idx:], token)
		if j < 0 {
			return false
		}
		start := idx + j
		end := start + len(token)
		if end < len(text) && !glued {
			if r, _ := utf8.DecodeRuneInString(text[end:]); IsWordRune(r) {
				idx = end
				continue
			}
		}
		if start > 0 {
			r := PrevRune(text, start)
			if (!glued && IsWordRune(r)) || r == '(' {
				idx = end
				continue
			}
		}
		return true
	}
}

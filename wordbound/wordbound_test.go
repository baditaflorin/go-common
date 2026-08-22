package wordbound

import (
	"regexp"
	"testing"
)

func TestIsWordRune(t *testing.T) {
	cases := map[rune]bool{
		'a': true, 'Z': true, '5': true,
		'ē': true, // Latin extended (Latvian)
		'ş': true, // Latin extended (Turkish)
		'Ü': true, // Latin extended (Estonian)
		'Α': true, // Greek
		'А': true, // Cyrillic
		'名': true, // Han
		' ': false, '.': false, ',': false, '-': false, '(': false,
	}
	for r, want := range cases {
		if got := IsWordRune(r); got != want {
			t.Errorf("IsWordRune(%q) = %v, want %v", r, got, want)
		}
	}
}

func TestPrevRune(t *testing.T) {
	s := "café"
	// "café" is c-a-f-é; é starts at byte offset 3 (2-byte encoding).
	if r := PrevRune(s, 3); r != 'f' {
		t.Errorf("PrevRune(%q, 3) = %q, want 'f'", s, r)
	}
	if r := PrevRune(s, len(s)); r != 'é' {
		t.Errorf("PrevRune(%q, len(s)) = %q, want 'é'", s, r)
	}
}

func TestIsGluedScript(t *testing.T) {
	cases := map[string]bool{
		"株式会社": true,
		"주식회사": true,
		"GmbH": false,
		"SL":   false,
		"ЕООД": false, // Cyrillic is NOT a glued script
	}
	for tok, want := range cases {
		if got := IsGluedScript(tok); got != want {
			t.Errorf("IsGluedScript(%q) = %v, want %v", tok, got, want)
		}
	}
}

// TestContainsToken_LatvianBusinessHoursFalsePositive pins the exact
// go_gdpr_compliance round-26/29 real-evidence bug this package fixes at
// the source: a naive `\bSL\b`-style regex match inside "Slēgts" ("Closed")
// must not be mistaken for a token match. ContainsToken never used `\b` in
// the first place, so this is a straightforward correctness check, not a
// regression test against a prior bug in this package — but it's the
// canonical real-world case that motivated this package, so it's pinned
// here as documentation-by-test.
func TestContainsToken_LatvianBusinessHoursFalsePositive(t *testing.T) {
	if ContainsToken("S.-Sv. Slēgts", "SL") {
		t.Errorf(`ContainsToken("S.-Sv. Slēgts", "SL") = true, want false (real Latvian business-hours text, not a company suffix)`)
	}
}

func TestContainsToken_RealSuffixEvidence(t *testing.T) {
	cases := []struct {
		text, token string
		want        bool
	}{
		{"EUROSTARS HOTEL COMPANY SL", "SL", true},
		{"Uponor Eesti OÜ", "OÜ", true},
		{"MAILOS ΑΕ", "ΑΕ", true},
		{"ОВО-БУЛ ЕООД", "ЕООД", true},
		{"株式会社ソニー", "株式会社", true}, // glued: prefix form, name runs on after
		{"Inca", "Inc", false},    // must not match inside a longer word
		{"Income", "Inc", false},
		{"ПАОЗТ", "АО", false},                                           // must not match inside a longer Cyrillic word
		{"Equity crowdfunding (Limited availability)", "Limited", false}, // parenthetical aside
	}
	for _, c := range cases {
		if got := ContainsToken(c.text, c.token); got != c.want {
			t.Errorf("ContainsToken(%q, %q) = %v, want %v", c.text, c.token, got, c.want)
		}
	}
}

// TestTrailingBoundary_FixesLatvianFalsePositive proves the regex-composition
// consumption style (splicing TrailingBoundary into a hand-built pattern)
// closes the same class of bug a bare `\b` misses.
func TestTrailingBoundary_FixesLatvianFalsePositive(t *testing.T) {
	broken := regexp.MustCompile(`(?i)\b[\p{L}][\p{L}0-9.,'-]*\s+SL\b`)
	fixed := regexp.MustCompile(`(?i)\b[\p{L}][\p{L}0-9.,'-]*\s+SL` + TrailingBoundary)
	if !broken.MatchString("S.-Sv. Slēgts") {
		t.Fatalf("test setup invalid: expected the bare-\\b pattern to reproduce the known false positive")
	}
	if fixed.MatchString("S.-Sv. Slēgts") {
		t.Errorf("pattern using TrailingBoundary still matches the Latvian false positive")
	}
	if !fixed.MatchString("EUROSTARS HOTEL COMPANY SL") {
		t.Errorf("pattern using TrailingBoundary must still match real SL evidence")
	}
}

// TestLeadingBoundary_FixesTurkishTruncation proves the regex-composition
// consumption style closes the go_update_frequency Turkish month-name
// truncation bug.
func TestLeadingBoundary_FixesTurkishTruncation(t *testing.T) {
	broken := regexp.MustCompile(`(?i)\b([\p{L}]{3,})\s+(\d{1,2}),\s+((?:19|20)\d{2})`)
	fixed := regexp.MustCompile(`(?i)` + LeadingBoundary + `([\p{L}]{3,})\s+(\d{1,2}),\s+((?:19|20)\d{2})`)

	if m := broken.FindStringSubmatch("Şubat 15, 2024"); m == nil || m[1] != "ubat" {
		t.Fatalf("test setup invalid: expected the bare-\\b pattern to reproduce the known truncation (got %v)", m)
	}
	m := fixed.FindStringSubmatch("Şubat 15, 2024")
	if m == nil {
		t.Fatalf("pattern using LeadingBoundary failed to match %q at all", "Şubat 15, 2024")
	}
	if m[1] != "Şubat" {
		t.Errorf("pattern using LeadingBoundary captured %q, want %q", m[1], "Şubat")
	}

	if m := fixed.FindStringSubmatch("August 14, 2024"); m == nil || m[1] != "August" {
		t.Errorf("pattern using LeadingBoundary must still match plain ASCII month names, got %v", m)
	}
}

package wordbound

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestContainsTokenEmptyToken pins the hang FuzzContainsToken found: an empty
// token used to loop forever on any text that starts with a word rune.
func TestContainsTokenEmptyToken(t *testing.T) {
	for _, text := range []string{"", "abc", "(abc", " abc", "株式会社"} {
		if ContainsToken(text, "") {
			t.Errorf("ContainsToken(%q, \"\") = true, want false", text)
		}
	}
}

// FuzzContainsToken scans arbitrary text for arbitrary tokens. It must
// terminate (the fuzzer treats a hang as a failure) and a hit must be a real
// substring.
func FuzzContainsToken(f *testing.F) {
	f.Add("Acme Inc", "Inc")
	f.Add("Income statement", "Inc")
	f.Add("ПАО Газпром", "АО")
	f.Add("株式会社ソニー", "株式会社")
	f.Add("(Limited availability)", "Limited")
	f.Add("abc", "")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, text, token string) {
		got := ContainsToken(text, token)
		if got && !strings.Contains(text, token) {
			t.Fatalf("ContainsToken(%q, %q) = true but token is not a substring", text, token)
		}
	})
}

// FuzzPrevRune checks PrevRune for every in-range offset.
func FuzzPrevRune(f *testing.F) {
	f.Add("héllo", 2)
	f.Add("日本語", 3)
	f.Add("", 0)
	f.Fuzz(func(t *testing.T, s string, pos int) {
		if pos < 0 {
			pos = -pos
		}
		if pos < 0 { // -MinInt
			return
		}
		pos %= len(s) + 1
		r := PrevRune(s, pos)
		if pos == 0 && r != utf8.RuneError {
			t.Fatalf("PrevRune(%q, 0) = %q, want RuneError", s, r)
		}
	})
}

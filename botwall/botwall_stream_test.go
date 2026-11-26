package botwall

import (
	"math/rand"
	"strings"
	"testing"
)

func TestStreamingVisibleProof(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		proof bool
	}{
		{"ascii", "<p>" + strings.Repeat("abc ", 1100) + "</p>", true},
		{"multilingual", "<p>" + strings.Repeat("ț", 1501) + "</p>", true},
		{"casefolded script", "<SCRIPT>" + strings.Repeat("x", 17000) + "</SCRIPT><p>" + strings.Repeat("a", 3001), true},
		{"unicode in script", "<sTyLe>ș</STyLE>" + strings.Repeat("a", 3001), true},
		{"unterminated script", "<script>" + strings.Repeat("a", 4000), false},
		{"invalid utf8", "\xff" + strings.Repeat("a", 4000), false},
		{"ascii case mapping", "K" + strings.Repeat("a", 4000), false},
		{"long head", "<script>" + strings.Repeat("x", 65<<10) + "</script>" + strings.Repeat("a", 4000), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasVisibleTextBeyondLimit(tc.doc); got != tc.proof {
				t.Fatalf("proof = %v; want %v", got, tc.proof)
			}
			if tc.proof && visibleTextLen(strings.ToLower(tc.doc)) <= MaxVisibleText {
				t.Fatal("proof must imply the original visible-text limit is exceeded")
			}
			if got, want := Detect(tc.doc), detectPlain(tc.doc); got != want {
				t.Fatalf("Detect = %q; want %q", got, want)
			}
		})
	}
}

func TestStreamingProofNeverExceedsFullCount(t *testing.T) {
	r := rand.New(rand.NewSource(20261126))
	parts := []string{
		"text ", "<div>", "</div>", "<SCRIPT>", "</sCrIpT>",
		"<style>", "</STYLE>", "<scrİpt>", "<script x='ș'>", "<", ">",
		"ș", "中文", "Ω", "K", "\xff", "\xe2\x82", " ", "\t", "&nbsp;",
		strings.Repeat("a", 1000), strings.Repeat("ă", 400),
	}
	proved := 0
	for i := 0; i < 10000; i++ {
		var doc strings.Builder
		for j, n := 0, 5+r.Intn(80); j < n; j++ {
			doc.WriteString(parts[r.Intn(len(parts))])
		}
		s := doc.String()
		if hasVisibleTextBeyondLimit(s) {
			proved++
			if visibleTextLen(strings.ToLower(s)) <= MaxVisibleText {
				t.Fatalf("false proof on generated document %d", i)
			}
		}
		if got, want := Detect(s), detectPlain(s); got != want {
			t.Fatalf("verdict mismatch on generated document %d", i)
		}
	}
	if proved == 0 {
		t.Fatal("generated cases did not exercise successful proofs")
	}
}

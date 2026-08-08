package apikey

import "testing"

func TestTierSatisfies(t *testing.T) {
	cases := []struct {
		name                 string
		callerTier, required string
		want                 bool
	}{
		{"no requirement, no caller tier", "", "", true},
		{"no requirement, caller has a tier", "vetted-pentest", "", true},
		{"exact match", "vetted-pentest", "vetted-pentest", true},
		{"mismatch", "free", "vetted-pentest", false},
		{"case-sensitive mismatch", "Vetted-Pentest", "vetted-pentest", false},
		{"empty caller tier against a requirement — must fail closed", "", "vetted-pentest", false},
		{"different non-empty tiers", "open", "free", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TierSatisfies(c.callerTier, c.required); got != c.want {
				t.Errorf("TierSatisfies(%q, %q) = %v, want %v", c.callerTier, c.required, got, c.want)
			}
		})
	}
}

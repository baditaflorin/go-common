package server

import "testing"

func TestKebabAlias(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"go_email_extractor", "email-extractor"},
		{"go_ad_density", "ad-density"},
		{"go_a11y_quick", "a11y-quick"},
		{"go_xss_scanner", "xss-scanner"},
		{"go_scc", "scc"},     // single-word stays single-word
		{"plain", ""},         // no go_ prefix, no underscores → no alias
		{"already-kebab", ""}, // no go_ prefix, no underscores → no alias
		{"snake_case_no_go_prefix", "snake-case-no-go-prefix"},
	}
	for _, c := range cases {
		got := KebabAlias(c.in)
		if got != c.want {
			t.Errorf("KebabAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

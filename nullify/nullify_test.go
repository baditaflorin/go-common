package nullify

import "testing"

func TestIsPlaceholder(t *testing.T) {
	cases := map[string]bool{
		"":             true,
		"  ":           true,
		"unknown":      true,
		"Unknown":      true,
		"UNKNOWN":      true,
		"n/a":          true,
		"N/A":          true,
		"none":         true,
		"-":            true,
		"tbd":          true,
		"not found":    true,
		"unavailable":  true,
		"Stripe":       false,
		"bootstrapped": false,
		"Education":    false,
	}
	for in, want := range cases {
		if got := IsPlaceholder(in); got != want {
			t.Errorf("IsPlaceholder(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStringOrNil(t *testing.T) {
	if got := StringOrNil("unknown"); got != nil {
		t.Errorf("StringOrNil(unknown) = %v, want nil", *got)
	}
	if got := StringOrNil("  "); got != nil {
		t.Errorf("StringOrNil(blank) = %v, want nil", *got)
	}
	got := StringOrNil("  Stripe  ")
	if got == nil || *got != "Stripe" {
		t.Fatalf("StringOrNil(Stripe) = %v, want \"Stripe\"", got)
	}
}

package apikey

import "testing"

func TestResolve_FirstNonEmptyWins(t *testing.T) {
	t.Setenv("A", "")
	t.Setenv("B", "from-b")
	t.Setenv("C", "from-c")
	r := Resolve("A", "B", "C")
	if !r.Found || r.Key != "from-b" || r.Source != "B" {
		t.Fatalf("got %+v, want Found=true Key=from-b Source=B", r)
	}
}

func TestResolve_AllUnsetReturnsNotFound(t *testing.T) {
	t.Setenv("X1_NOT_SET", "")
	t.Setenv("X2_NOT_SET", "")
	r := Resolve("X1_NOT_SET", "X2_NOT_SET")
	if r.Found || r.Key != "" || r.Source != "" {
		t.Fatalf("got %+v, want zero ResolveResult", r)
	}
}

func TestResolve_EmptyVarListReturnsNotFound(t *testing.T) {
	r := Resolve()
	if r.Found || r.Key != "" || r.Source != "" {
		t.Fatalf("got %+v, want zero ResolveResult", r)
	}
}

func TestResolve_EmptyValueSkipped(t *testing.T) {
	// An env var set to "" should be treated as unset so callers can
	// blank-disable a variable without re-ordering the chain.
	t.Setenv("FIRST", "")
	t.Setenv("SECOND", "wins")
	r := Resolve("FIRST", "SECOND")
	if r.Source != "SECOND" || r.Key != "wins" {
		t.Fatalf("got %+v, want SECOND=wins", r)
	}
}

func TestHasFleetPrefix(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"dynamic ak_", "ak_a1b2c3d4e5f6", true},
		{"fallback fb_", "fb_static_demo_value", true},
		{"empty", "", false},
		{"public demo token", "default_token", false},
		{"misconfigured truthy", "true", false},
		{"misconfigured yes", "yes", false},
		{"foreign prefix (e.g. AWS)", "AKIA1234567890", false},
		{"prefix only, no body", "ak_", true}, // shape-only; keystore rejects empties
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasFleetPrefix(c.key); got != c.want {
				t.Fatalf("HasFleetPrefix(%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

func TestKeyPrefixConstants_StableValues(t *testing.T) {
	// Pin the published prefix values as a contract. Changing either
	// breaks every consumer's startup shape check until they bump
	// go-common — should be a deliberate, coordinated migration.
	if KeyPrefixDynamic != "ak_" {
		t.Fatalf("KeyPrefixDynamic changed unexpectedly: %q", KeyPrefixDynamic)
	}
	if KeyPrefixFallback != "fb_" {
		t.Fatalf("KeyPrefixFallback changed unexpectedly: %q", KeyPrefixFallback)
	}
}


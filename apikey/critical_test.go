package apikey

import (
	"strings"
	"testing"
)

func TestResolveCritical_Unset(t *testing.T) {
	// Ensure neither env var is set.
	t.Setenv("CRIT_TEST_A", "")
	t.Setenv("CRIT_TEST_B", "")
	_, err := ResolveCritical("svc-x", "CRIT_TEST_A", "CRIT_TEST_B")
	if err == nil {
		t.Fatal("expected error for unset envs, got nil")
	}
	for _, want := range []string{
		"apikey.critical_key_missing",
		"slug=svc-x",
		"env=CRIT_TEST_A,CRIT_TEST_B",
		"reason=unset",
		"fix=`fleet-runner key issue svc-x",
		CriticalRunbookURL,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nfull error: %s", want, err)
		}
	}
}

func TestResolveCritical_DemoTokenRejected(t *testing.T) {
	t.Setenv("CRIT_TEST_KEY", "default_token")
	_, err := ResolveCritical("svc-y", "CRIT_TEST_KEY")
	if err == nil {
		t.Fatal("expected error for demo token, got nil")
	}
	for _, want := range []string{
		"slug=svc-y",
		"env=CRIT_TEST_KEY",
		"reason=demo_default_token",
		"fleet-runner key issue svc-y",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nfull error: %s", want, err)
		}
	}
}

func TestResolveCritical_UnknownPrefix(t *testing.T) {
	t.Setenv("CRIT_TEST_KEY", "AKIAIOSFODNN7EXAMPLE") // looks like AWS, not fleet
	_, err := ResolveCritical("svc-z", "CRIT_TEST_KEY")
	if err == nil {
		t.Fatal("expected error for unknown prefix, got nil")
	}
	if !strings.Contains(err.Error(), "reason=unknown_prefix") {
		t.Errorf("expected unknown_prefix reason; got: %s", err)
	}
	// Error must reference both recognised prefixes so operators
	// can sanity-check the value they're trying to use.
	if !strings.Contains(err.Error(), KeyPrefixDynamic) || !strings.Contains(err.Error(), KeyPrefixFallback) {
		t.Errorf("expected recognised prefixes in error; got: %s", err)
	}
}

func TestResolveCritical_Success_Dynamic(t *testing.T) {
	want := "ak_" + strings.Repeat("a", 64)
	t.Setenv("CRIT_TEST_KEY", want)
	got, err := ResolveCritical("svc-ok", "CRIT_TEST_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveCritical_Success_Fallback(t *testing.T) {
	// Static fallback keys (fb_...) are explicitly recognised — they
	// are issued for break-glass scenarios and remain auditable in
	// the keystore, unlike the universal demo token.
	want := "fb_some_static_value"
	t.Setenv("CRIT_TEST_KEY", want)
	got, err := ResolveCritical("svc-ok", "CRIT_TEST_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveCritical_ChainPrecedence(t *testing.T) {
	// First non-empty wins, just like Resolve. Validation runs on
	// the winner only — earlier entries being "default_token" is
	// irrelevant if a later var carries a real key.
	t.Setenv("CRIT_TEST_A", "")
	t.Setenv("CRIT_TEST_B", "ak_realkey0123")
	got, err := ResolveCritical("svc-ok", "CRIT_TEST_A", "CRIT_TEST_B")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ak_realkey0123" {
		t.Errorf("got %q, want ak_realkey0123", got)
	}
}

func TestResolveCritical_ProgrammerErrors(t *testing.T) {
	if _, err := ResolveCritical("", "FOO"); err == nil {
		t.Error("expected error for empty slug")
	}
	if _, err := ResolveCritical("svc"); err == nil {
		t.Error("expected error for empty envVars")
	}
}

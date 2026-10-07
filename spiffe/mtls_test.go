package spiffe

import (
	"testing"
	"time"
)

func TestParseIDsRequiresExactUniqueSPIFFEIDs(t *testing.T) {
	if _, err := parseIDs(nil); err == nil {
		t.Fatal("empty peer allowlist must fail closed")
	}
	if _, err := parseIDs([]string{"spiffe://example.org/service/api", "spiffe://example.org/service/api"}); err == nil {
		t.Fatal("duplicate peer identity must be rejected")
	}
	if _, err := parseIDs([]string{"spiffe://example.org/service/api/"}); err == nil {
		t.Fatal("non-canonical SPIFFE identity must be rejected")
	}
	if _, err := parseIDs([]string{"spiffe://example.org/service/api", "spiffe://example.org/service/broker"}); err != nil {
		t.Fatalf("exact identity allowlist rejected: %v", err)
	}
}

func TestHTTPClientTimeoutIsBoundedBeforeWorkloadAPIConnect(t *testing.T) {
	ctx := t.Context()
	for _, timeout := range []time.Duration{0, time.Millisecond, 3*time.Minute + time.Second} {
		if _, err := NewHTTPClientWithTimeout(ctx, "unix:///unused.sock", []string{"spiffe://example.org/service/api"}, timeout); err == nil {
			t.Fatalf("timeout %s was accepted", timeout)
		}
	}
}

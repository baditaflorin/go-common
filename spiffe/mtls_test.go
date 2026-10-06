package spiffe

import "testing"

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

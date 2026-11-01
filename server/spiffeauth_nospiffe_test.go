//go:build gocommon_nospiffe

package server

import (
	"strings"
	"testing"
)

func TestSPIFFEAuthNamesTheTagItWasBuiltWith(t *testing.T) {
	var lines []string
	logf := func(f string, _ ...any) { lines = append(lines, f) }
	t.Setenv("SPIFFEAUTH_MODE", "")
	if startSPIFFEAuth(nil, logf) != nil || len(lines) != 0 {
		t.Fatal("unset mode must be silent")
	}
	t.Setenv("SPIFFEAUTH_MODE", "dual")
	if startSPIFFEAuth(nil, logf) != nil || len(lines) != 1 || !strings.Contains(lines[0], "gocommon_nospiffe") {
		t.Fatalf("lines = %v", lines)
	}
}

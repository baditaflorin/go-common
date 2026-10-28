//go:build gocommon_nopyroscope

package server

import (
	"strings"
	"testing"
)

// (captureLog is the helper of egress_rule_test.go.)
//
// A binary built with -tags gocommon_nopyroscope profiles nothing; it says so when the
// variable that asks for profiling is set, and is silent when it is not.
func TestStartProfilingSaysItCannotWhenAskedTo(t *testing.T) {
	t.Setenv("PYROSCOPE_SERVER_ADDRESS", "https://pyroscope.example")
	buf := captureLog(t)
	stop := startProfiling("go_company_size")
	stop()
	if !strings.Contains(buf.String(), "gocommon_nopyroscope") {
		t.Fatalf("log = %q; want a line naming the tag", buf.String())
	}
}

func TestStartProfilingIsSilentWhenNotAskedTo(t *testing.T) {
	t.Setenv("PYROSCOPE_SERVER_ADDRESS", "")
	buf := captureLog(t)
	startProfiling("go_company_size")()
	if buf.Len() != 0 {
		t.Fatalf("log = %q; want nothing", buf.String())
	}
}

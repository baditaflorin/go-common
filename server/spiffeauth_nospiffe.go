//go:build gocommon_nospiffe

package server

import (
	"os"
	"strings"

	"github.com/baditaflorin/go-common/middleware"
)

// startSPIFFEAuth in a binary built with -tags gocommon_nospiffe: the Workload
// API client is not linked, so the SPIFFE identity path does not exist. Without
// SPIFFEAUTH_MODE a service sees no difference; with it set, one line says why
// nothing happens.
func startSPIFFEAuth(_ middleware.IdentityObserver, logf func(string, ...any)) *identityRuntime {
	if m := strings.ToLower(strings.TrimSpace(os.Getenv("SPIFFEAUTH_MODE"))); m != "" && m != "off" {
		logf("server: SPIFFEAUTH_MODE is set but this binary was built without the SPIFFE Workload API client " +
			"(-tags gocommon_nospiffe): rebuild without the tag or unset it; running with API keys only")
	}
	return nil
}

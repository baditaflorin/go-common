//go:build gocommon_nospiffe

package server

import "github.com/baditaflorin/go-common/fleetfetch"

// connectFleetfetchIdentity in a binary built with -tags gocommon_nospiffe: the
// Workload API client is not linked, so the identity path does not exist.
func connectFleetfetchIdentity(_ string, logf func(string, ...any)) {
	logf("server: %s=dual is set but this binary was built without the SPIFFE Workload API client "+
		"(-tags gocommon_nospiffe): rebuild without the tag or unset it; the fetch cache is called with the API key only", fleetfetch.EnvSPIFFE)
}

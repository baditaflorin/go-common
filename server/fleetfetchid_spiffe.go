//go:build !gocommon_nospiffe

package server

import (
	"context"
	"time"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/spiffe"
)

// connectFleetfetchIdentity opens the Workload API JWT source in the background and,
// once it answers, installs it as the default fleetfetch identity. A binary built with
// -tags gocommon_nospiffe does not link package spiffe: see fleetfetchid_nospiffe.go.
func connectFleetfetchIdentity(socket string, logf func(string, ...any)) {
	if !spiffe.ValidateSocket(socket) {
		logf("server: %s=dual but SPIFFE_ENDPOINT_SOCKET is not a unix:// Workload API address, the fetch cache is called with the API key only", fleetfetch.EnvSPIFFE)
		return
	}
	go func() {
		for delay := time.Second; ; delay = min(delay*2, time.Minute) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			src, err := spiffe.New(ctx, socket)
			cancel()
			if err == nil {
				fleetfetch.SetDefaultIdentity(src, "")
				logf("server: fetch cache calls carry a JWT-SVID next to the API key (%s=dual)", fleetfetch.EnvSPIFFE)
				return
			}
			time.Sleep(delay) // the Workload API is not there (yet): the key carries every call meanwhile
		}
	}()
}

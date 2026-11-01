package server

import (
	"log"
	"os"
	"strings"
	"sync"

	"github.com/baditaflorin/go-common/fleetfetch"
)

// The SPIFFE identity of this service's calls to the fetch cache (fleetfetch
// identity.go), wired from the environment.
//
// With FLEET_FETCH_CACHE_SPIFFE unset or off nothing here runs: no Workload API
// connection, no header, no metric series. With dual, every fleetfetch client in
// the process adds an audience-scoped JWT-SVID to its cache calls NEXT TO its API
// key. The Workload API is reached in the background, so start-up never waits for
// SPIRE: until it answers, calls go out on the key alone. A misconfiguration is
// logged and the service runs on its key; it never stops a service from starting.

var fleetfetchIDOnce sync.Once

// startFleetfetchIdentity installs the process-wide identity source once.
func startFleetfetchIdentity() {
	fleetfetchIDOnce.Do(func() {
		switch m := strings.ToLower(strings.TrimSpace(os.Getenv(fleetfetch.EnvSPIFFE))); m {
		case "", "off":
			return
		case "dual":
			connectFleetfetchIdentity(os.Getenv("SPIFFE_ENDPOINT_SOCKET"), log.Printf)
		default:
			log.Printf("server: %s=%q is not off or dual, the fetch cache is called with the API key only", fleetfetch.EnvSPIFFE, m)
		}
	})
}

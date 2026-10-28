//go:build gocommon_nopyroscope

package server

import (
	"log"
	"os"
)

// startProfiling is a no-op in a binary built with -tags gocommon_nopyroscope. It says so
// once, loudly, when PYROSCOPE_SERVER_ADDRESS asks for profiling, so a deploy that turns
// profiling on for a binary that cannot do it does not fail silently.
func startProfiling(string) func() {
	if os.Getenv("PYROSCOPE_SERVER_ADDRESS") != "" {
		log.Printf("server: PYROSCOPE_SERVER_ADDRESS is set but this binary was built with -tags gocommon_nopyroscope: no profile is sent")
	}
	return func() {}
}

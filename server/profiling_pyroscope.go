//go:build !gocommon_nopyroscope

package server

import (
	"log"

	"github.com/baditaflorin/go-common/profiling"
)

// startProfiling starts continuous profiling when PYROSCOPE_SERVER_ADDRESS is set and
// returns the function that stops it. A binary built with -tags gocommon_nopyroscope does
// not link package profiling (the Pyroscope client): see profiling_nopyroscope.go.
func startProfiling(appName string) func() {
	return startServerProfiling(appName, profiling.StartFromEnv)
}

func startServerProfiling(appName string, start func(string) (profiling.StopFunc, error)) profiling.StopFunc {
	serviceName := appName
	if alias := KebabAlias(appName); alias != "" {
		serviceName = alias
	}
	stop, err := start(serviceName)
	if err != nil {
		log.Printf("server: profiling startup error: %v", err)
		return func() {}
	}
	if stop == nil {
		return func() {}
	}
	return stop
}

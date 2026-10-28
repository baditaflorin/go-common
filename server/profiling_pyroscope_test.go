//go:build !gocommon_nopyroscope

package server

import (
	"testing"

	"github.com/baditaflorin/go-common/profiling"
)

func TestServerStartProfilingUsesStablePublicServiceName(t *testing.T) {
	started, stopped := "", 0
	stop := startServerProfiling("go_company_size", func(serviceName string) (profiling.StopFunc, error) {
		started = serviceName
		return func() { stopped++ }, nil
	})
	if started != "company-size" {
		t.Fatalf("profiling service name = %q, want company-size", started)
	}
	stop()
	if stopped != 1 {
		t.Fatalf("profiler stop calls = %d, want 1", stopped)
	}
}

func TestServerStartProfilingUsesExistingSlugWhenNoAliasIsNeeded(t *testing.T) {
	started := ""
	stop := startServerProfiling("analyze-headers", func(serviceName string) (profiling.StopFunc, error) {
		started = serviceName
		return func() {}, nil
	})
	stop()
	if started != "analyze-headers" {
		t.Fatalf("profiling service name = %q, want analyze-headers", started)
	}
}

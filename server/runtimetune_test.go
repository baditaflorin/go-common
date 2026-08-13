package server

import (
	"runtime/debug"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/runtimetune"
)

// TestNewAppliesRuntimeTuning is the fleet-wide wiring assertion: every
// service reaches server.New (directly or through server.Run), so New is
// where the container-aware GC/memory tuning has to be triggered. Nothing
// else in go-common calls runtimetune.Apply, so observing Applied()==true
// after a New proves the linkage.
func TestNewAppliesRuntimeTuning(t *testing.T) {
	cfg := &config.Config{AppName: "runtimetune_wiring", Version: "0.0.0", Port: "0"}
	_ = New(cfg)

	res, ok := runtimetune.Applied()
	if !ok {
		t.Fatal("server.New did not apply runtime tuning")
	}

	// Whatever it decided, it must leave the runtime in a usable state:
	// GOGC either off (-1, operator's choice) or a positive percentage,
	// and GOMEMLIMIT strictly positive.
	gogc := debug.SetGCPercent(-1)
	debug.SetGCPercent(gogc)
	if gogc == 0 {
		t.Fatalf("GOGC left at 0 after server.New (result: %s)", res.LogLine())
	}
	if limit := debug.SetMemoryLimit(-1); limit <= 0 {
		t.Fatalf("GOMEMLIMIT left at %d after server.New (result: %s)", limit, res.LogLine())
	}

	// Repeated New calls must not re-tune.
	_ = New(cfg)
	res2, _ := runtimetune.Applied()
	if res != res2 {
		t.Fatalf("runtime tuning re-applied on the second New: %+v vs %+v", res, res2)
	}
}

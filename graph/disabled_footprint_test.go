package graph

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// A subprocess makes the retained-heap and goroutine check independent of
// other tests' singleton state, senders and HTTP transports.
func TestDisabledGraphNoBackgroundWork(t *testing.T) {
	if os.Getenv("GO_COMMON_GRAPH_DISABLED_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDisabledGraphNoBackgroundWork$", "-test.v")
		cmd.Env = append(os.Environ(), "GO_COMMON_GRAPH_DISABLED_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("disabled graph subprocess: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	t.Setenv("GRAPH_ENABLED", "false")
	t.Setenv("GRAPH_API_KEY", "")
	t.Setenv("GRAPH_API_KEY_FILE", "")
	t.Setenv("GRAPH_READER_API_KEY", "")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	beforeG := runtime.NumGoroutine()
	Init("disabled-footprint-test", "test")
	Record(Event{Direction: "out", Target: "example.com"})
	if Enabled() || Stats().EventsRecorded != 0 || ServiceID() != "disabled-footprint-test" {
		t.Fatal("disabled graph must retain identity without recording events")
	}
	if err := Shutdown(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	added := runtime.NumGoroutine() - beforeG
	fmt.Printf("disabled_graph_heap_delta_bytes=%d goroutines_added=%d\n", delta, added)
	if delta > 128<<10 {
		t.Fatalf("disabled graph retains %d bytes; want at most 128 KiB", delta)
	}
	if added != 0 {
		t.Fatalf("disabled graph added %d goroutines", added)
	}
	if s := ensureInit(); s.ring != nil || s.sender != nil || s.rng != nil {
		t.Fatal("disabled graph allocated writer resources")
	}
}

func BenchmarkDisabledGraphBootstrap(b *testing.B) {
	b.Setenv("GRAPH_ENABLED", "false")
	b.Setenv("GRAPH_API_KEY", "")
	b.Setenv("GRAPH_API_KEY_FILE", "")
	b.ReportAllocs()
	for b.Loop() {
		s := bootstrap("disabled-bench", "test", true)
		if s.sender != nil || s.ring != nil {
			b.Fatal("disabled writer allocated resources")
		}
	}
}

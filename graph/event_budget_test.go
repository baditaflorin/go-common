package graph

import (
	"strings"
	"testing"
	"unsafe"
)

func TestRecordDropsOversizedFieldsBeforeRetention(t *testing.T) {
	resetState(t)
	setGraphEnv(t, unreachableCollectorURL)
	t.Cleanup(func() { _ = Shutdown() })
	Init("service", "test")
	for _, field := range []string{"direction", "caller", "target", "path", "method"} {
		e := Event{Direction: "out", Caller: "caller", Target: "target", Path: "/ok", Method: "GET"}
		long := strings.Repeat("x", MaxEventStringBytes+1)
		switch field {
		case "direction":
			e.Direction = long
		case "caller":
			e.Caller = long
		case "target":
			e.Target = long
		case "path":
			e.Path = long
		case "method":
			e.Method = long
		}
		Record(e)
	}
	if got := Stats(); got.EventsDroppedOversized != 5 || got.EventsRecorded != 0 || got.EventsDropped != 0 {
		t.Fatalf("oversize counters = %+v", got)
	}
	if got := ensureInit().ring.len(); got != 0 {
		t.Fatalf("ring retains %d oversized events", got)
	}
	// The budget is aggregate and includes the identity Record fills in.
	Record(Event{Direction: "out", Target: "target", Method: "GET", Path: strings.Repeat("p", MaxEventStringBytes)})
	if Stats().EventsDroppedOversized != 6 {
		t.Fatal("aggregate string budget was not enforced")
	}
	Record(Event{Direction: "out", Target: "target", Method: "GET", Path: "/ok"})
	if Stats().EventsRecorded != 1 {
		t.Fatal("ordinary event was dropped")
	}
}

func TestRetainedEventDoesNotAliasLargeInputBacking(t *testing.T) {
	backing := strings.Repeat("x", 1<<20)
	short := backing[:3]
	got := ownEventStrings(Event{Direction: short, Caller: short, Target: short, Path: short, Method: short}, "service")
	for name, value := range map[string]string{"direction": got.Direction, "caller": got.Caller, "target": got.Target, "path": got.Path, "method": got.Method} {
		if value != short || unsafe.StringData(value) == unsafe.StringData(short) {
			t.Errorf("retained %s aliases oversized input backing or changed content", name)
		}
	}
	serviceID := strings.Clone("service")
	// An identical identity from a request substring must resolve to the
	// process's existing allocation; common direction/method constants also
	// need no per-event copies.
	identityBacking := "service" + strings.Repeat("q", 1<<20)
	got = ownEventStrings(Event{Caller: identityBacking[:7], Target: serviceID, Direction: "out", Method: "GET"}, serviceID)
	if unsafe.StringData(got.Caller) != unsafe.StringData(serviceID) || unsafe.StringData(got.Target) != unsafe.StringData(serviceID) {
		t.Fatal("process identity was copied or request backing retained")
	}
}

func TestEventRetentionBudgetBoundary(t *testing.T) {
	e := Event{Direction: "out", Caller: "caller", Target: "target", Method: "GET"}
	e.Path = strings.Repeat("p", MaxEventStringBytes-len(e.Direction)-len(e.Caller)-len(e.Target)-len(e.Method))
	if !eventFitsRetentionBudget(e) {
		t.Fatal("exact budget must fit")
	}
	e.Path += "p"
	if eventFitsRetentionBudget(e) {
		t.Fatal("one byte over budget must not fit")
	}
}

func BenchmarkEventRetentionBudget(b *testing.B) {
	e := Event{Direction: "out", Caller: "caller", Target: "target", Path: "/api/{id}", Method: "GET"}
	b.ReportAllocs()
	for b.Loop() {
		if !eventFitsRetentionBudget(e) {
			b.Fatal("unexpected rejection")
		}
	}
}

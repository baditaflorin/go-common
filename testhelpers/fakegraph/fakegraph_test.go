package fakegraph_test

import (
	"testing"

	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/testhelpers/fakegraph"
)

func TestRecorderFiltersByDirection(t *testing.T) {
	r := fakegraph.New()
	r.Record(graph.Event{Direction: "out", Caller: "svc-a", Target: "svc-b"})
	r.Record(graph.Event{Direction: "out", Caller: "svc-a", Target: "svc-c"})
	r.Record(graph.Event{Direction: "in", Caller: "svc-z", Target: "svc-a"})

	if r.Len() != 3 || len(r.Events()) != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	if got := r.OutboundTo("svc-b"); len(got) != 1 || got[0].Caller != "svc-a" {
		t.Fatalf("OutboundTo(svc-b) = %+v", got)
	}
	if got := r.InboundFrom("svc-z"); len(got) != 1 || got[0].Target != "svc-a" {
		t.Fatalf("InboundFrom(svc-z) = %+v", got)
	}
	if got := r.OutboundTo("svc-z"); len(got) != 0 {
		t.Fatalf("OutboundTo(svc-z) = %+v, want none (that one is inbound)", got)
	}
}

func TestEventsReturnsACopyAndResetClears(t *testing.T) {
	r := fakegraph.New()
	r.Record(graph.Event{Target: "x"})
	got := r.Events()
	got[0].Target = "mutated"
	if r.Events()[0].Target != "x" {
		t.Fatal("mutating the slice returned by Events changed the recorder")
	}
	r.Reset()
	if r.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", r.Len())
	}
}

package degraded

import (
	"sync"
	"testing"
)

func TestAppendAndSlice(t *testing.T) {
	s := New()
	s.Append("html-proxy-down")
	s.Append("keystore-degraded")
	got := s.Slice()
	if len(got) != 2 || got[0] != "html-proxy-down" || got[1] != "keystore-degraded" {
		t.Fatalf("Slice = %v", got)
	}
}

func TestEmptyTokenIgnored(t *testing.T) {
	s := New()
	s.Append("")
	if len(s.Slice()) != 0 {
		t.Fatal("empty token should be ignored")
	}
}

func TestObserverSplitsPrimitiveAndSuffix(t *testing.T) {
	var events []Event
	var mu sync.Mutex
	SetDefaultObserver(observerFunc(func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer SetDefaultObserver(nil)

	s := New()
	s.Append("html-proxy-down")
	s.Append("nodash")

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Primitive != "html-proxy" || events[0].Suffix != "down" {
		t.Fatalf("split = %+v", events[0])
	}
	if events[1].Primitive != "nodash" || events[1].Suffix != "" {
		t.Fatalf("no-dash split = %+v", events[1])
	}
}

func TestAsSlicePtr(t *testing.T) {
	s := New()
	ptr := s.AsSlicePtr()
	*ptr = append(*ptr, "via-ptr")
	got := s.Slice()
	if len(got) != 1 || got[0] != "via-ptr" {
		t.Fatalf("Slice = %v", got)
	}
}

type observerFunc func(Event)

func (f observerFunc) ObserveDegraded(ev Event) { f(ev) }

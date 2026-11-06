package graph

import "testing"

func TestDrainClearsOnlyConsumedSlots(t *testing.T) {
	r := newRing(3)
	for _, path := range []string{"/first", "/second", "/third", "/fourth"} {
		r.push(Event{Direction: "out", Caller: "caller", Target: "target", Path: path, Method: "GET"})
	}
	// The ring has wrapped and evicted /first. Drain across the end of the
	// backing array while keeping /fourth queued in slot zero.
	got := r.drain(2)
	if len(got) != 2 || got[0].Path != "/second" || got[1].Path != "/third" {
		t.Fatalf("drained events = %+v", got)
	}
	if r.buf[1] != (Event{}) || r.buf[2] != (Event{}) {
		t.Fatal("consumed ring slots retain event references")
	}
	if r.buf[0].Path != "/fourth" || r.len() != 1 {
		t.Fatal("drain cleared a queued event")
	}
	last := r.drain(0)
	if len(last) != 1 || last[0].Path != "/fourth" {
		t.Fatalf("last drain = %+v", last)
	}
	for i, e := range r.buf {
		if e != (Event{}) {
			t.Fatalf("empty ring slot %d retains %+v", i, e)
		}
	}
}

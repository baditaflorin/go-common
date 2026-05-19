package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

func TestTripsAfterThreshold(t *testing.T) {
	b := New(Options{Upstream: "x", FailureThreshold: 3, OpenFor: 50 * time.Millisecond})
	for i := 0; i < 3; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow %d should pass when closed: %v", i, err)
		}
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("Allow while open: %v, want ErrOpen", err)
	}
}

func TestHalfOpenProbeSuccessCloses(t *testing.T) {
	b := New(Options{Upstream: "x", FailureThreshold: 1, OpenFor: 20 * time.Millisecond})
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Failure()
	if b.State() != StateOpen {
		t.Fatalf("expected open, got %v", b.State())
	}
	time.Sleep(25 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("probe should pass: %v", err)
	}
	b.Success()
	if b.State() != StateClosed {
		t.Fatalf("after probe success: state = %v, want closed", b.State())
	}
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	b := New(Options{Upstream: "x", FailureThreshold: 1, OpenFor: 20 * time.Millisecond})
	_ = b.Allow()
	b.Failure()
	time.Sleep(25 * time.Millisecond)
	_ = b.Allow() // probe
	b.Failure()
	if b.State() != StateOpen {
		t.Fatalf("probe failure should reopen, got %v", b.State())
	}
}

func TestObserverFires(t *testing.T) {
	var events []Event
	SetDefaultObserver(observerFunc(func(ev Event) { events = append(events, ev) }))
	defer SetDefaultObserver(nil)

	b := New(Options{Upstream: "y", FailureThreshold: 1, OpenFor: 10 * time.Millisecond})
	_ = b.Allow()
	b.Failure() // closed -> open
	if got := len(events); got != 1 {
		t.Fatalf("events after trip = %d, want 1", got)
	}
	if events[0].To != StateOpen || events[0].Reason != "threshold_reached" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

type observerFunc func(Event)

func (f observerFunc) ObserveCircuit(ev Event) { f(ev) }

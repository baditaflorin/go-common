package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHalfOpenAdmitsExactlyOneConcurrentProbe(t *testing.T) {
	b := New(Options{FailureThreshold: 1, OpenFor: time.Hour})
	b.Failure()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := b.Allow(); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrOpen) {
				t.Errorf("Allow: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("admitted %d probes, want 1", got)
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state %v, want half-open while probe runs", got)
	}
	b.Success()
	if got := b.State(); got != StateClosed {
		t.Fatalf("successful probe left state %v", got)
	}
}

func TestLateSuccessWhileOpenDoesNotCloseBreaker(t *testing.T) {
	b := New(Options{FailureThreshold: 1, OpenFor: time.Hour})
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Failure() // Another previously admitted call is still in flight.
	b.Success()
	if got := b.State(); got != StateOpen {
		t.Fatalf("late success changed open state to %v", got)
	}
}

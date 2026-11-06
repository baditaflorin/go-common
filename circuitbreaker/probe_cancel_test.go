package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

func TestClosedAdmissionHasNoProbeCancellation(t *testing.T) {
	b := New(Options{})
	cancel, err := b.AllowWithCancel()
	if err != nil || cancel != nil {
		t.Fatalf("closed admission cancel=%v err=%v", cancel != nil, err)
	}
}

func TestProbeCancellationCannotReleaseLaterAdmission(t *testing.T) {
	b := New(Options{FailureThreshold: 1, OpenFor: time.Hour})
	b.Failure()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	first, err := b.AllowWithCancel()
	if err != nil || first == nil {
		t.Fatalf("first admission: %v", err)
	}
	first()
	second, err := b.AllowWithCancel()
	if err != nil || second == nil {
		t.Fatalf("replacement admission: %v", err)
	}
	first() // An old deferred cleanup must not release the replacement probe.
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("old callback changed state to %v", got)
	}
	if _, err := b.AllowWithCancel(); !errors.Is(err, ErrOpen) {
		t.Fatalf("competitor err=%v", err)
	}
	b.Success()
	second()
	if got := b.State(); got != StateClosed {
		t.Fatalf("completed callback reopened breaker: %v", got)
	}
}

func BenchmarkAllowWithCancelClosed(b *testing.B) {
	breaker := New(Options{})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cancel, err := breaker.AllowWithCancel()
		if err != nil || cancel != nil {
			b.Fatal("unexpected probe")
		}
	}
}

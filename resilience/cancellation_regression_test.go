package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/resilience"
	"github.com/baditaflorin/go-common/retry"
)

func TestAlreadyCanceledCallDoesNotAttemptOrRefillBudget(t *testing.T) {
	budget := resilience.NewBudget(1, 0, resilience.WithBurst(1))
	if !budget.TryWithdraw() {
		t.Fatal("initial budget missing")
	}
	p := resilience.New("cancel", resilience.WithBudget(budget))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := p.Do(ctx, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if got := budget.Tokens(); got != 0 {
		t.Fatalf("canceled call deposited %v tokens", got)
	}
	if got := p.Stats().Attempts; got != 0 {
		t.Fatalf("canceled call counted %d attempts", got)
	}
}

func TestCancellationFromZeroBackoffStopsNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := retry.Transient(errors.New("upstream"))
	p := resilience.New("cancel", resilience.WithoutBudget(), resilience.WithBackoff(func(int) time.Duration { cancel(); return 0 }))
	calls := 0
	err := p.Do(ctx, func(context.Context) error { calls++; return upstream })
	if calls != 1 || !errors.Is(err, upstream) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

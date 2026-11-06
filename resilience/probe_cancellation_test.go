package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/resilience"
	"github.com/baditaflorin/go-common/retry"
)

func TestCanceledRecoveryProbeAllowsLaterRecovery(t *testing.T) {
	for _, cancelCaller := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelCaller), func(t *testing.T) {
			breaker := circuitbreaker.New(circuitbreaker.Options{FailureThreshold: 1, OpenFor: time.Nanosecond})
			breaker.Failure()
			budget := resilience.NewBudget(0, 0, resilience.WithBurst(1))
			policy := resilience.New("probe", resilience.WithBreaker(breaker), resilience.WithBudget(budget), resilience.WithBackoff(retry.NoBackoff()), resilience.WithRetryIf(func(error) bool { return true }))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := policy.Do(ctx, func(context.Context) error {
				calls++
				if cancelCaller {
					cancel()
				}
				return fmt.Errorf("attempt: %w", context.Canceled)
			})
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("canceled call err=%v attempts=%d", err, calls)
			}
			if got := budget.Tokens(); got != 1 {
				t.Fatalf("cancellation consumed retry budget: %v", got)
			}
			if got := breaker.State(); got != circuitbreaker.StateOpen {
				t.Fatalf("canceled probe left state=%v, want neutral open", got)
			}
			calls = 0
			err = policy.Do(context.Background(), func(context.Context) error { calls++; return nil })
			if err != nil || calls != 1 || breaker.State() != circuitbreaker.StateClosed {
				t.Fatalf("recovery err=%v attempts=%d state=%v", err, calls, breaker.State())
			}
		})
	}
}

func TestCanceledOldClosedCallCannotReleaseCurrentRecoveryProbe(t *testing.T) {
	breaker := circuitbreaker.New(circuitbreaker.Options{FailureThreshold: 1, OpenFor: time.Nanosecond})
	policy := resilience.New("probe", resilience.WithBreaker(breaker), resilience.WithMaxAttempts(1))
	oldCtx, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	oldStarted, oldRelease, oldDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		oldDone <- policy.Do(oldCtx, func(context.Context) error { close(oldStarted); <-oldRelease; return oldCtx.Err() })
	}()
	<-oldStarted
	breaker.Failure()
	probeStarted, probeRelease, probeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		probeDone <- policy.Do(context.Background(), func(context.Context) error { close(probeStarted); <-probeRelease; return nil })
	}()
	select {
	case <-probeStarted:
	case err := <-probeDone:
		t.Fatalf("probe refused: %v", err)
	}
	cancelOld()
	close(oldRelease)
	if err := <-oldDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("old result: %v", err)
	}
	if got := breaker.State(); got != circuitbreaker.StateHalfOpen {
		t.Errorf("old cancellation changed probe state to %v", got)
	}
	err := policy.Do(context.Background(), func(context.Context) error { t.Error("competitor entered while probe running"); return nil })
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Errorf("competing result=%v", err)
	}
	close(probeRelease)
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
	if breaker.State() != circuitbreaker.StateClosed {
		t.Fatal("successful current probe did not close breaker")
	}
}

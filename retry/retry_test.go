package retry_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/retry"
)

func TestDoSuccess(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func(_ context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDoRetriesAndSucceeds(t *testing.T) {
	var calls atomic.Int32
	err := retry.Do(context.Background(), func(_ context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errors.New("transient")
		}
		return nil
	},
		retry.WithMaxAttempts(5),
		retry.WithBackoff(retry.NoBackoff()),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoExhausts(t *testing.T) {
	calls := 0
	err := retry.Do(context.Background(), func(_ context.Context) error {
		calls++
		return errors.New("always fails")
	},
		retry.WithMaxAttempts(3),
		retry.WithBackoff(retry.NoBackoff()),
	)
	if err == nil {
		t.Fatal("expected error after exhausted attempts")
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDoContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before first call

	err := retry.Do(ctx, func(_ context.Context) error {
		return errors.New("should not reach")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDoRetryIfAborts(t *testing.T) {
	sentinel := errors.New("permanent")
	calls := 0
	err := retry.Do(context.Background(), func(_ context.Context) error {
		calls++
		return sentinel
	},
		retry.WithMaxAttempts(5),
		retry.WithBackoff(retry.NoBackoff()),
		retry.WithRetryIf(func(err error) bool { return false }),
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (abort immediately), got %d", calls)
	}
}

func TestConstantBackoff(t *testing.T) {
	b := retry.ConstantBackoff(50 * time.Millisecond)
	for i := 1; i <= 5; i++ {
		if d := b(i); d != 50*time.Millisecond {
			t.Fatalf("attempt %d: got %v, want 50ms", i, d)
		}
	}
}

func TestLinearBackoff(t *testing.T) {
	b := retry.LinearBackoff(10 * time.Millisecond)
	if d := b(3); d != 30*time.Millisecond {
		t.Fatalf("got %v, want 30ms", d)
	}
}

func TestExponentialJitterRange(t *testing.T) {
	b := retry.ExponentialJitter(10 * time.Millisecond)
	// Run multiple samples and verify they fall in the expected range.
	for i := 0; i < 100; i++ {
		d := b(3) // attempt 3 → ceiling = 10ms × 4 = 40ms
		if d < 0 || d > 40*time.Millisecond {
			t.Fatalf("attempt 3: got %v, want [0, 40ms]", d)
		}
	}
}

func TestTransient(t *testing.T) {
	cause := errors.New("io error")
	transient := retry.Transient(cause)
	if !retry.IsTransient(transient) {
		t.Fatal("Transient error should be IsTransient")
	}
	if retry.IsTransient(cause) {
		t.Fatal("plain error should not be IsTransient")
	}
}

func TestDoMaxAttemptsOne(t *testing.T) {
	calls := 0
	retry.Do(context.Background(), func(_ context.Context) error {
		calls++
		return errors.New("fail")
	}, retry.WithMaxAttempts(1), retry.WithBackoff(retry.NoBackoff()))
	if calls != 1 {
		t.Fatalf("MaxAttempts=1: expected 1 call, got %d", calls)
	}
}

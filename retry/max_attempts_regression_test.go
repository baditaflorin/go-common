package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestWithMaxAttemptsClampsInvalidCountsToOne(t *testing.T) {
	failure := errors.New("failure")
	for _, maximum := range []int{-10, -1, 0, 1, 3} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			calls := 0
			err := Do(context.Background(), func(context.Context) error { calls++; return failure }, WithMaxAttempts(maximum), WithBackoff(NoBackoff()))
			want := max(1, maximum)
			if calls != want || !errors.Is(err, failure) {
				t.Fatalf("calls=%d err=%v, want %d failures", calls, err, want)
			}
		})
	}
}

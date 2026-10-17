package resilience_test

import (
	"context"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/resilience"
)

// The cost of a call that succeeds first time, which is nearly every call.
func BenchmarkPolicyDoSuccess(b *testing.B) {
	p := resilience.New("bench")
	ctx := context.Background()
	fn := func(context.Context) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Do(ctx, fn)
	}
}

func BenchmarkPolicyDoSuccessWithBreakerAndShed(b *testing.B) {
	p := resilience.New("bench",
		resilience.WithBreaker(circuitbreaker.New(circuitbreaker.Options{Upstream: "bench", OpenFor: time.Hour})),
		resilience.WithShed(loadshed.New("bench", 1<<20)))
	ctx := context.Background()
	fn := func(context.Context) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Do(ctx, fn)
	}
}

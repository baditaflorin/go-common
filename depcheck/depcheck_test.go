package depcheck

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEmptyRegistry(t *testing.T) {
	r := New()
	if got := r.Snapshot(context.Background()); got != nil {
		t.Fatalf("empty registry should return nil, got %+v", got)
	}
	if !AllOK(nil) {
		t.Fatalf("AllOK(nil) should be true")
	}
}

func TestMixedSuccess(t *testing.T) {
	r := New()
	r.Register("ok-dep", func(ctx context.Context) error { return nil })
	r.Register("bad-dep", func(ctx context.Context) error { return errors.New("boom") })

	got := r.Snapshot(context.Background())
	if len(got) != 2 {
		t.Fatalf("want 2 statuses, got %d", len(got))
	}
	if !got[0].OK || got[0].Name != "ok-dep" {
		t.Fatalf("ok-dep failed: %+v", got[0])
	}
	if got[1].OK || got[1].Name != "bad-dep" || got[1].Error != "boom" {
		t.Fatalf("bad-dep wrong: %+v", got[1])
	}
	if AllOK(got) {
		t.Fatalf("AllOK should be false when one dep failed")
	}
}

func TestProbeTimeout(t *testing.T) {
	r := New().WithTimeout(50 * time.Millisecond)
	r.Register("slow", func(ctx context.Context) error {
		select {
		case <-time.After(500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	start := time.Now()
	got := r.Snapshot(context.Background())
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Fatalf("Snapshot should respect per-probe timeout, took %v", elapsed)
	}
	if got[0].OK {
		t.Fatalf("slow dep should have failed: %+v", got[0])
	}
	if got[0].LatencyMs < 40 || got[0].LatencyMs > 200 {
		t.Fatalf("latency_ms suspicious: %d", got[0].LatencyMs)
	}
}

func TestParallelism(t *testing.T) {
	r := New().WithTimeout(time.Second)
	for i := 0; i < 5; i++ {
		r.Register("dep", func(ctx context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		})
	}
	start := time.Now()
	r.Snapshot(context.Background())
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("5 parallel 100ms probes should finish in ~100ms, took %v", elapsed)
	}
}

func TestStatusFields(t *testing.T) {
	r := New()
	r.Register("dep", func(ctx context.Context) error { return nil })
	got := r.Snapshot(context.Background())
	if got[0].CheckedAt.IsZero() {
		t.Fatalf("CheckedAt should be set")
	}
	if got[0].LatencyMs < 0 {
		t.Fatalf("LatencyMs should be non-negative")
	}
}

package clock_test

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/clock"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestRealNow(t *testing.T) {
	c := clock.Real()
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real().Now() = %v outside [%v, %v]", got, before, after)
	}
}

func TestRealSince(t *testing.T) {
	c := clock.Real()
	past := time.Now().Add(-10 * time.Millisecond)
	if d := c.Since(past); d <= 0 {
		t.Fatalf("Since returned non-positive %v", d)
	}
}

func TestMockNow(t *testing.T) {
	mc := clock.NewMock(epoch)
	if got := mc.Now(); !got.Equal(epoch) {
		t.Fatalf("got %v, want %v", got, epoch)
	}
}

func TestMockAdvance(t *testing.T) {
	mc := clock.NewMock(epoch)
	mc.Advance(5 * time.Minute)
	want := epoch.Add(5 * time.Minute)
	if got := mc.Now(); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMockSince(t *testing.T) {
	mc := clock.NewMock(epoch)
	past := epoch.Add(-3 * time.Second)
	if d := mc.Since(past); d != 3*time.Second {
		t.Fatalf("got %v, want 3s", d)
	}
}

func TestMockAfterFires(t *testing.T) {
	mc := clock.NewMock(epoch)
	ch := mc.After(1 * time.Second)
	mc.Advance(500 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("fired too early")
	default:
	}
	mc.Advance(500 * time.Millisecond)
	select {
	case got := <-ch:
		want := epoch.Add(1 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("channel value = %v, want %v", got, want)
		}
	default:
		t.Fatal("timer did not fire after Advance past deadline")
	}
}

func TestMockSet(t *testing.T) {
	mc := clock.NewMock(epoch)
	target := epoch.Add(10 * time.Hour)
	mc.Set(target)
	if got := mc.Now(); !got.Equal(target) {
		t.Fatalf("got %v, want %v", got, target)
	}
}

func TestMockAfterAlreadyPast(t *testing.T) {
	mc := clock.NewMock(epoch)
	// After(0) or negative should fire immediately
	ch := mc.After(0)
	select {
	case <-ch:
	default:
		t.Fatal("After(0) did not fire immediately")
	}
}

func TestMockAdvanceNegativeNoOp(t *testing.T) {
	mc := clock.NewMock(epoch)
	mc.Advance(-5 * time.Minute)
	if got := mc.Now(); !got.Equal(epoch) {
		t.Fatalf("negative Advance should be no-op, got %v", got)
	}
}

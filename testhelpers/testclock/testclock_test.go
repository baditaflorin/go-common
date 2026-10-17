package testclock_test

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/testhelpers/testclock"
)

func TestEpochIsFixedAndAdvances(t *testing.T) {
	c := testclock.Epoch()
	want := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !c.Now().Equal(want) {
		t.Fatalf("Epoch().Now() = %v, want %v", c.Now(), want)
	}
	c.Advance(time.Hour)
	if got := c.Since(want); got != time.Hour {
		t.Fatalf("Since after Advance(1h) = %v", got)
	}
}

func TestNewStartsWhereTold(t *testing.T) {
	start := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	if got := testclock.New(start).Now(); !got.Equal(start) {
		t.Fatalf("New(start).Now() = %v, want %v", got, start)
	}
}

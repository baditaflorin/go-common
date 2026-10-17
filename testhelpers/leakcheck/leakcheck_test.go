package leakcheck

import (
	"strings"
	"testing"
	"time"
)

func TestLeakedFindsABlockedGoroutineUntilItExits(t *testing.T) {
	before := Take()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-stop
	}()

	got := Leaked(before, 100*time.Millisecond)
	if len(got) != 1 || !strings.Contains(got[0], "TestLeakedFindsABlockedGoroutineUntilItExits") {
		t.Fatalf("Leaked = %v, want the one blocked goroutine", got)
	}

	close(stop)
	<-done
	if got := Leaked(before, 2*time.Second); len(got) != 0 {
		t.Fatalf("Leaked after the goroutine exited = %v, want none", got)
	}
}

func TestIgnoreSkipsMatchingGoroutines(t *testing.T) {
	before := Take()
	stop := make(chan struct{})
	go func() { <-stop }()
	t.Cleanup(func() { close(stop) })

	if got := Leaked(before, 50*time.Millisecond, Ignore("TestIgnoreSkipsMatchingGoroutines")); len(got) != 0 {
		t.Fatalf("an ignored goroutine was reported: %v", got)
	}
}

func TestCheckPassesWhenNothingLeaks(t *testing.T) {
	Check(t)
	done := make(chan struct{})
	go func() { close(done) }()
	<-done
}

func TestLeakedWaitsForGoroutinesThatAreWindingDown(t *testing.T) {
	before := Take()
	go func() { time.Sleep(150 * time.Millisecond) }()
	if got := Leaked(before, 3*time.Second); len(got) != 0 {
		t.Fatalf("a goroutine that exits within the wait was reported: %v", got)
	}
}

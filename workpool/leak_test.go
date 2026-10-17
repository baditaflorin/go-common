package workpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/testhelpers/leakcheck"
)

// Wait and Close must leave nothing running, including the goroutines of tasks
// that were queued behind a full pool.
func TestPoolLeavesNoGoroutines(t *testing.T) {
	leakcheck.Check(t)

	p := New("leak", 4)
	var ran atomic.Int64
	for i := 0; i < 50; i++ {
		if err := p.Submit(func() {
			ran.Add(1)
			time.Sleep(time.Millisecond)
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	p.Wait()
	p.Close()
	if got := ran.Load(); got != 50 {
		t.Fatalf("ran %d tasks, want 50", got)
	}
}

// A caller whose context fires while it waits for a slot must not leave its
// goroutine behind.
func TestSubmitCtxCancelLeavesNoGoroutines(t *testing.T) {
	leakcheck.Check(t)

	p := New("leak-cancel", 1)
	release := make(chan struct{})
	if err := p.Submit(func() { <-release }); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := p.SubmitCtx(ctx, func() {}); err == nil {
		t.Fatal("SubmitCtx on a full pool with an expiring context returned nil")
	}

	close(release)
	p.Close()
}

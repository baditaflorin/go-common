package workpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedConcurrency(t *testing.T) {
	p := New("t", 2)
	var inflight, peak atomic.Int64
	var done atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		_ = p.Submit(func() {
			defer wg.Done()
			cur := inflight.Add(1)
			for {
				pk := peak.Load()
				if cur <= pk || peak.CompareAndSwap(pk, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inflight.Add(-1)
			done.Add(1)
		})
	}
	wg.Wait()
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", got)
	}
	if got := done.Load(); got != 5 {
		t.Fatalf("done = %d, want 5", got)
	}
}

func TestTrySubmitSheds(t *testing.T) {
	p := New("shed", 1)
	release := make(chan struct{})
	if !p.TrySubmit(func() { <-release }) {
		t.Fatal("first TrySubmit should succeed")
	}
	if p.TrySubmit(func() {}) {
		t.Fatal("second TrySubmit should fail (pool full)")
	}
	close(release)
	p.Wait()
}

func TestSubmitCtxCancel(t *testing.T) {
	p := New("cancel", 1)
	release := make(chan struct{})
	_ = p.Submit(func() { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := p.SubmitCtx(ctx, func() {})
	if err == nil {
		t.Fatal("SubmitCtx should error when ctx fires while waiting")
	}
	close(release)
	p.Wait()
}

func TestObserverFires(t *testing.T) {
	phases := map[Phase]int{}
	var mu sync.Mutex
	SetDefaultObserver(observerFunc(func(ev Event) {
		mu.Lock()
		phases[ev.Phase]++
		mu.Unlock()
	}))
	defer SetDefaultObserver(nil)

	p := New("obs", 1)
	_ = p.Submit(func() {})
	p.Wait()
	mu.Lock()
	defer mu.Unlock()
	if phases[PhaseStarted] != 1 || phases[PhaseFinished] != 1 {
		t.Fatalf("phases = %v, want started=1 finished=1", phases)
	}
}

type observerFunc func(Event)

func (f observerFunc) ObserveWorkpool(ev Event) { f(ev) }

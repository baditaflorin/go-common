package workpool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForPoolCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("pool condition timed out")
		}
		runtime.Gosched()
	}
}

func TestCloseWakesQueuedSubmittersBeforeActiveTaskFinishes(t *testing.T) {
	p := New("close", 1)
	release := make(chan struct{})
	defer close(release)
	if err := p.Submit(func() { <-release }); err != nil {
		t.Fatal(err)
	}
	submitted := make(chan error, 1)
	go func() {
		submitted <- p.SubmitCtx(context.Background(), func() { t.Error("queued task started after close") })
	}()
	waitForPoolCondition(t, func() bool { return p.QueueDepth() == 1 })
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	waitForPoolCondition(t, p.isClosed)
	select {
	case err := <-submitted:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("SubmitCtx: %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close left a submitter blocked behind the active task")
	}
	if got := p.QueueDepth(); got != 0 {
		t.Fatalf("queue depth %d after close", got)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before active task finished")
	default:
	}
}

func TestConcurrentCloseNeverReturnsBeforeAdmittedTasks(t *testing.T) {
	for range 2000 {
		p := New("race", 1)
		var closeReturned atomic.Bool
		var late atomic.Bool
		var callers sync.WaitGroup
		callers.Add(2)
		start := make(chan struct{})
		go func() {
			defer callers.Done()
			<-start
			p.TrySubmit(func() {
				if closeReturned.Load() {
					late.Store(true)
				}
			})
		}()
		go func() { defer callers.Done(); <-start; p.Close(); closeReturned.Store(true) }()
		close(start)
		callers.Wait()
		p.Wait()
		if late.Load() {
			t.Fatal("admitted task started after Close returned")
		}
	}
}

func TestCanceledContextDoesNotAdmitTask(t *testing.T) {
	p := New("canceled", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.SubmitCtx(ctx, func() { t.Error("canceled task started") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("SubmitCtx: %v", err)
	}
	p.Close()
}

func TestAdmissionObserverCanReadCloseState(t *testing.T) {
	p := New("observer", 1)
	SetDefaultObserver(observerFunc(func(Event) { _ = p.isClosed() }))
	defer SetDefaultObserver(nil)
	done := make(chan struct{})
	go func() { p.TrySubmit(func() {}); p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer ran under admission lock")
	}
}

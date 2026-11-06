package workpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClosedQueuedSubmitEmitsTerminalObserverEvent(t *testing.T) {
	p := New("closed-queue", 1)
	release := make(chan struct{})
	if err := p.Submit(func() { <-release }); err != nil {
		t.Fatal(err)
	}
	terminal := make(chan Event, 1)
	SetDefaultObserver(observerFunc(func(e Event) {
		if e.Phase == PhaseCanceled {
			_ = p.isClosed()
			terminal <- e
		}
	}))
	t.Cleanup(func() { close(release); p.Close(); SetDefaultObserver(nil) })
	result := make(chan error, 1)
	go func() { result <- p.SubmitCtx(context.Background(), func() { t.Error("closed task admitted") }) }()
	waitForPoolCondition(t, func() bool { return p.QueueDepth() == 1 })
	go p.Close()
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("SubmitCtx error = %v", err)
	}
	select {
	case e := <-terminal:
		if e.QueueDepth != 0 || e.InFlight != 1 {
			t.Fatalf("terminal state = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("closed queued submitter did not emit terminal state")
	}
}

func TestPostSlotRejectionEmitsTerminalObserverEvent(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "context"
		if closed {
			name = "pool"
		}
		t.Run(name, func(t *testing.T) {
			p := New("post-slot", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queued := make(chan struct{})
			terminal := make(chan Event, 1)
			SetDefaultObserver(observerFunc(func(e Event) {
				if e.Phase == PhaseQueued {
					// Hold the admission lock until the submitter has reserved a
					// slot, so cancellation/close reaches the second check.
					p.closedMu.Lock()
					close(queued)
				}
				if e.Phase == PhaseCanceled {
					_ = p.isClosed()
					terminal <- e
				}
			}))
			defer SetDefaultObserver(nil)
			result := make(chan error, 1)
			go func() { result <- p.SubmitCtx(ctx, func() { t.Error("rejected task admitted") }) }()
			<-queued
			waitForPoolCondition(t, func() bool { return len(p.sem) == 1 })
			if closed {
				p.closed = true
				close(p.done)
			} else {
				cancel()
			}
			p.closedMu.Unlock()
			want := error(context.Canceled)
			if closed {
				want = ErrClosed
			}
			if err := <-result; !errors.Is(err, want) {
				t.Fatalf("SubmitCtx error = %v, want %v", err, want)
			}
			select {
			case e := <-terminal:
				if e.QueueDepth != 0 || e.InFlight != 0 {
					t.Fatalf("terminal state = %+v", e)
				}
			case <-time.After(time.Second):
				t.Fatal("post-slot rejection did not emit terminal state")
			}
			p.Close()
		})
	}
}

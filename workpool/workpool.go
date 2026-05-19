// Package workpool is a bounded-concurrency goroutine pool used by
// every fleet service that fans out work (subdomain-finder,
// broken-links, iframe-analyzer's recursive walker, …). Today each
// reimplements its own semaphore + WaitGroup; this package centralises
// the shape and emits in-flight + queue-depth metrics via the
// Observer hook.
//
// Semantics:
//
//   - New(name, size) returns a pool with `size` worker slots.
//   - Submit(task) blocks until a slot is free, then runs task in a
//     goroutine. SubmitCtx blocks bounded by ctx.
//   - TrySubmit(task) returns false immediately if no slot is free —
//     useful for shedding load.
//   - Wait() blocks until every submitted task has finished.
//   - Close() prevents further submits and waits for in-flight tasks.
//
// The pool is intentionally minimal: no priority queue, no
// per-task timeout, no retries — those are the caller's job. The
// pool's only responsibility is "at most N tasks run at once."
package workpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Pool is a bounded-concurrency worker pool. Construct with New;
// safe for concurrent Submit calls.
type Pool struct {
	name string
	sem  chan struct{}
	wg   sync.WaitGroup

	closedMu sync.Mutex
	closed   bool

	inFlight   atomic.Int64
	queueDepth atomic.Int64 // callers parked in Submit waiting for a slot
}

// ErrClosed is returned by Submit/TrySubmit after Close has been called.
var ErrClosed = errors.New("workpool: closed")

// New constructs a pool with `size` concurrent slots. Name is the
// pool slug ("crawler", "fanout-resolver") used as a metric label —
// keep it short and stable.
func New(name string, size int) *Pool {
	if size <= 0 {
		size = 1
	}
	if name == "" {
		name = "_unnamed"
	}
	return &Pool{
		name: name,
		sem:  make(chan struct{}, size),
	}
}

// Name returns the pool slug.
func (p *Pool) Name() string { return p.name }

// Size returns the configured concurrency limit.
func (p *Pool) Size() int { return cap(p.sem) }

// InFlight returns the current number of running tasks.
func (p *Pool) InFlight() int64 { return p.inFlight.Load() }

// QueueDepth returns the current number of Submit callers blocked
// waiting for a slot.
func (p *Pool) QueueDepth() int64 { return p.queueDepth.Load() }

// Submit blocks until a slot is available, then starts task in a
// goroutine. Returns ErrClosed if the pool has been closed.
func (p *Pool) Submit(task func()) error {
	return p.SubmitCtx(context.Background(), task)
}

// SubmitCtx is Submit bounded by ctx. If ctx fires while waiting for
// a slot, ctx.Err() is returned and task is not started.
func (p *Pool) SubmitCtx(ctx context.Context, task func()) error {
	if task == nil {
		return nil
	}
	if p.isClosed() {
		return ErrClosed
	}
	p.queueDepth.Add(1)
	emit(Event{Pool: p.name, Phase: PhaseQueued, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		p.queueDepth.Add(-1)
		emit(Event{Pool: p.name, Phase: PhaseCanceled, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
		return ctx.Err()
	}
	p.queueDepth.Add(-1)
	if p.isClosed() {
		<-p.sem
		return ErrClosed
	}
	p.wg.Add(1)
	p.inFlight.Add(1)
	emit(Event{Pool: p.name, Phase: PhaseStarted, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
	go func() {
		defer func() {
			<-p.sem
			p.inFlight.Add(-1)
			p.wg.Done()
			emit(Event{Pool: p.name, Phase: PhaseFinished, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
		}()
		task()
	}()
	return nil
}

// TrySubmit starts task only if a slot is immediately available.
// Returns true on success, false if the pool is full or closed.
func (p *Pool) TrySubmit(task func()) bool {
	if task == nil || p.isClosed() {
		return false
	}
	select {
	case p.sem <- struct{}{}:
	default:
		emit(Event{Pool: p.name, Phase: PhaseShed, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
		return false
	}
	p.wg.Add(1)
	p.inFlight.Add(1)
	emit(Event{Pool: p.name, Phase: PhaseStarted, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
	go func() {
		defer func() {
			<-p.sem
			p.inFlight.Add(-1)
			p.wg.Done()
			emit(Event{Pool: p.name, Phase: PhaseFinished, InFlight: p.inFlight.Load(), QueueDepth: p.queueDepth.Load()})
		}()
		task()
	}()
	return true
}

// Wait blocks until every submitted task has finished. Does NOT close
// the pool — further Submits are allowed afterwards.
func (p *Pool) Wait() {
	p.wg.Wait()
}

// Close prevents further submits and waits for in-flight tasks to
// finish. Safe to call multiple times.
func (p *Pool) Close() {
	p.closedMu.Lock()
	p.closed = true
	p.closedMu.Unlock()
	p.wg.Wait()
}

func (p *Pool) isClosed() bool {
	p.closedMu.Lock()
	defer p.closedMu.Unlock()
	return p.closed
}

// Phase buckets the observer event types.
type Phase string

const (
	PhaseQueued   Phase = "queued"   // a Submit caller is waiting for a slot
	PhaseStarted  Phase = "started"  // a task obtained a slot and began running
	PhaseFinished Phase = "finished" // a task returned and released its slot
	PhaseCanceled Phase = "canceled" // a Submit caller's ctx fired before getting a slot
	PhaseShed     Phase = "shed"     // TrySubmit refused because no slot was free
)

// Observer receives one event per Submit / TrySubmit lifecycle phase.
// Implementations MUST NOT block.
type Observer interface {
	ObserveWorkpool(Event)
}

// Event is the per-phase payload handed to an Observer.
type Event struct {
	Pool       string
	Phase      Phase
	InFlight   int64
	QueueDepth int64
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer. Pass nil to
// disable. Wired by promx.AutoWire.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer or nil.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func emit(ev Event) {
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveWorkpool(ev)
	}
}

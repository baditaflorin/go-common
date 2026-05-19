// Package clock provides a Clock interface with a real implementation
// (backed by time.Now) and a controllable mock implementation for
// deterministic tests.
//
// Any package that calls time.Now() or time.Since() directly cannot
// be tested deterministically. By accepting a Clock the package
// becomes trivially testable: inject clock.Real() in production,
// clock.NewMock(t0) in tests, advance with mock.Advance(d).
//
// Usage — production:
//
//	type Worker struct { clk clock.Clock }
//	func New() *Worker { return &Worker{clk: clock.Real()} }
//	func (w *Worker) Age(t time.Time) time.Duration { return w.clk.Since(t) }
//
// Usage — test:
//
//	mc := clock.NewMock(time.Unix(0, 0))
//	w  := &Worker{clk: mc}
//	mc.Advance(5 * time.Minute)
//	// w.Age(...) now returns a deterministic value
package clock

import (
	"sync"
	"time"
)

// Clock is the interface satisfied by both Real and Mock.
// Any code that needs testable time handling should accept a Clock.
type Clock interface {
	// Now returns the current (or mocked) time.
	Now() time.Time
	// Since returns the time elapsed since t (equivalent to Now().Sub(t)).
	Since(t time.Time) time.Duration
	// After returns a channel that fires after duration d. On the real
	// clock this is time.After; on the mock it fires when the mock
	// time is advanced past the deadline.
	After(d time.Duration) <-chan time.Time
}

// ─── Real clock ────────────────────────────────────────────────────────────

type realClock struct{}

// Real returns the singleton real-time Clock backed by time.Now.
// This is what production code should use.
func Real() Clock { return realClock{} }

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ─── Mock clock ────────────────────────────────────────────────────────────

// Mock is a controllable Clock for use in tests. All methods are safe
// for concurrent use. Advance moves the clock forward, firing any
// timers whose deadline has been reached.
type Mock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewMock returns a Mock clock initialised to start. Use time.Unix(0,0)
// or a known UTC instant for fully deterministic tests.
func NewMock(start time.Time) *Mock {
	return &Mock{now: start}
}

// Now returns the mock's current time.
func (m *Mock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// Since returns m.Now().Sub(t).
func (m *Mock) Since(t time.Time) time.Duration {
	return m.Now().Sub(t)
}

// After returns a channel that will receive the mock time once
// m.Advance moves the clock past m.Now().Add(d).
func (m *Mock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan time.Time, 1)
	deadline := m.now.Add(d)
	if !m.now.Before(deadline) {
		// already past
		ch <- m.now
		return ch
	}
	m.waiters = append(m.waiters, waiter{deadline: deadline, ch: ch})
	return ch
}

// Advance moves the mock clock forward by d and fires any timers
// whose deadline has been reached. d must be positive; negative
// values are a no-op.
func (m *Mock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
	remaining := m.waiters[:0]
	for _, w := range m.waiters {
		if !m.now.Before(w.deadline) {
			w.ch <- m.now
		} else {
			remaining = append(remaining, w)
		}
	}
	m.waiters = remaining
}

// Set moves the mock clock to an absolute time. Fires any timers
// whose deadline ≤ t.
func (m *Mock) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = t
	remaining := m.waiters[:0]
	for _, w := range m.waiters {
		if !t.Before(w.deadline) {
			w.ch <- t
		} else {
			remaining = append(remaining, w)
		}
	}
	m.waiters = remaining
}

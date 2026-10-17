package resilience

import (
	"sync"
	"time"

	"github.com/baditaflorin/go-common/clock"
)

// Budget limits retries to a fraction of the calls made. It is a token bucket
// with two sources: every call deposits Ratio of a token, and the bucket also
// refills at MinPerSec tokens a second so that a low-traffic caller can still
// retry. A retry withdraws one token and is refused when none is left.
//
// Under a total outage this caps retries at about Ratio of the call rate plus
// MinPerSec, instead of multiplying the load by MaxAttempts.
type Budget struct {
	clk       clock.Clock
	ratio     float64
	minPerSec float64
	burst     float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// BudgetOption adjusts a Budget.
type BudgetOption func(*Budget)

// WithBudgetClock replaces the clock, for tests.
func WithBudgetClock(c clock.Clock) BudgetOption { return func(b *Budget) { b.clk = c } }

// WithBurst sets the bucket size: the most retries that can be taken in a burst.
func WithBurst(n float64) BudgetOption { return func(b *Budget) { b.burst = n } }

// Defaults for NewDefaultBudget.
const (
	DefaultRatio     = 0.1
	DefaultMinPerSec = 1.0
	DefaultBurst     = 10.0
)

// NewBudget returns a budget that deposits ratio tokens per call and refills at
// minPerSec tokens a second, holding at most 10 tokens unless WithBurst says
// otherwise. It starts full.
func NewBudget(ratio, minPerSec float64, opts ...BudgetOption) *Budget {
	b := &Budget{clk: clock.Real(), ratio: ratio, minPerSec: minPerSec, burst: DefaultBurst}
	for _, o := range opts {
		o(b)
	}
	if b.burst < 1 {
		b.burst = 1
	}
	b.tokens = b.burst
	b.last = b.clk.Now()
	return b
}

// NewDefaultBudget is NewBudget(DefaultRatio, DefaultMinPerSec).
func NewDefaultBudget(opts ...BudgetOption) *Budget {
	return NewBudget(DefaultRatio, DefaultMinPerSec, opts...)
}

func (b *Budget) refillLocked() {
	now := b.clk.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.minPerSec
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
}

// Deposit records one call. A nil Budget accepts everything.
func (b *Budget) Deposit() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	b.tokens += b.ratio
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// TryWithdraw takes the token for one retry and reports whether there was one.
// A nil Budget never refuses.
func (b *Budget) TryWithdraw() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Tokens returns the tokens currently available, for metrics and tests.
func (b *Budget) Tokens() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	return b.tokens
}

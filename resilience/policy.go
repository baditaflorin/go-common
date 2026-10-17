package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/retry"
)

// ErrShed is returned when the shed gate refused a call: too many are in flight.
var ErrShed = errors.New("resilience: shed, too many calls in flight")

// ErrCircuitOpen is returned when the breaker is open and the call was not made.
var ErrCircuitOpen = errors.New("resilience: circuit open")

// Policy runs calls through a shed gate, a breaker and a budgeted retry loop. It
// is safe for concurrent use; create one per upstream and share it.
type Policy struct {
	name        string
	maxAttempts int
	backoff     retry.BackoffFunc
	retryIf     func(error) bool
	budget      *Budget
	breaker     *circuitbreaker.Breaker
	shed        *loadshed.Gate

	calls, attempts, retries, budgetExhausted, shedCount, circuitOpen atomic.Int64
}

// Option configures a Policy.
type Option func(*Policy)

// WithMaxAttempts sets the total attempts per call, the first included.
func WithMaxAttempts(n int) Option { return func(p *Policy) { p.maxAttempts = n } }

// WithBackoff sets the wait before retry n (see retry.ExponentialJitter).
func WithBackoff(b retry.BackoffFunc) Option { return func(p *Policy) { p.backoff = b } }

// WithRetryIf replaces Retryable as the decision on whether to retry an error.
func WithRetryIf(f func(error) bool) Option { return func(p *Policy) { p.retryIf = f } }

// WithBudget replaces the default retry budget.
func WithBudget(b *Budget) Option { return func(p *Policy) { p.budget = b } }

// WithoutBudget removes the retry budget, so retries are limited only by
// MaxAttempts. That is the failure mode this package exists to avoid; use it only
// where the call rate is tiny and bounded.
func WithoutBudget() Option { return func(p *Policy) { p.budget = nil } }

// WithBreaker puts a circuit breaker in front of the attempts.
func WithBreaker(b *circuitbreaker.Breaker) Option { return func(p *Policy) { p.breaker = b } }

// WithShed puts a concurrency gate in front of the call.
func WithShed(g *loadshed.Gate) Option { return func(p *Policy) { p.shed = g } }

// New returns a Policy with 3 attempts, full-jitter exponential backoff from
// 100 ms, Retryable as the retry decision and a default retry budget.
func New(name string, opts ...Option) *Policy {
	p := &Policy{
		name:        name,
		maxAttempts: 3,
		backoff:     retry.ExponentialJitter(100 * time.Millisecond),
		retryIf:     Retryable,
		budget:      NewDefaultBudget(),
	}
	for _, o := range opts {
		o(p)
	}
	if p.maxAttempts < 1 {
		p.maxAttempts = 1
	}
	return p
}

// Name is the label the policy was created with.
func (p *Policy) Name() string { return p.name }

// Do makes the call through the policy. fn may run several times, so it must be
// safe to repeat; build the request inside it.
func (p *Policy) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	p.calls.Add(1)

	if p.shed != nil {
		release, ok := p.shed.TryAcquire()
		if !ok {
			p.shedCount.Add(1)
			return fmt.Errorf("%s: %w", p.name, ErrShed)
		}
		defer release()
	}
	p.budget.Deposit()

	var err error
	for attempt := 1; ; attempt++ {
		if p.breaker != nil {
			if berr := p.breaker.Allow(); berr != nil {
				p.circuitOpen.Add(1)
				if err != nil {
					// An earlier attempt of this call opened the breaker: the
					// failure that did it is the more useful error to report.
					return err
				}
				return fmt.Errorf("%s: %w", p.name, ErrCircuitOpen)
			}
		}

		p.attempts.Add(1)
		err = fn(ctx)
		p.record(ctx, err)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || attempt >= p.maxAttempts || !p.retryIf(err) {
			return err
		}
		if !p.budget.TryWithdraw() {
			p.budgetExhausted.Add(1)
			return err
		}
		p.retries.Add(1)

		if wait := p.backoff(attempt); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return err
			case <-t.C:
			}
		}
	}
}

// record tells the breaker how the attempt went. Only failures that look like
// upstream trouble count against it; an answer such as 404 shows it is healthy,
// and a caller's own cancellation or expired deadline says nothing reliable.
func (p *Policy) record(ctx context.Context, err error) {
	if p.breaker == nil {
		return
	}
	switch {
	case err == nil:
		p.breaker.Success()
	case ctx.Err() != nil:
		// The caller gave up or ran out of time: not reliable evidence either way.
	case Retryable(err):
		p.breaker.Failure()
	default:
		p.breaker.Success()
	}
}

// Stats is a snapshot of what a Policy has done.
type Stats struct {
	Calls           int64 // calls to Do
	Attempts        int64 // times fn ran
	Retries         int64 // retries taken (attempts beyond each call's first)
	BudgetExhausted int64 // retries refused by the budget
	Shed            int64 // calls refused by the shed gate
	CircuitOpen     int64 // attempts refused by the open breaker
}

// Stats returns the counters.
func (p *Policy) Stats() Stats {
	return Stats{
		Calls: p.calls.Load(), Attempts: p.attempts.Load(), Retries: p.retries.Load(),
		BudgetExhausted: p.budgetExhausted.Load(), Shed: p.shedCount.Load(), CircuitOpen: p.circuitOpen.Load(),
	}
}

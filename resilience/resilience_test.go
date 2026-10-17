package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/clock"
	fleeterrors "github.com/baditaflorin/go-common/errors"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/resilience"
	"github.com/baditaflorin/go-common/retry"
)

// flaky is an error that says it is worth retrying.
type flaky struct{ retry bool }

func (e flaky) Error() string   { return "flaky" }
func (e flaky) Retryable() bool { return e.retry }

var (
	errRetry = flaky{retry: true}
	errFatal = flaky{retry: false}
)

func newPolicy(opts ...resilience.Option) *resilience.Policy {
	return resilience.New("test", append([]resilience.Option{resilience.WithBackoff(retry.NoBackoff())}, opts...)...)
}

func counting(errs ...error) (func(context.Context) error, *atomic.Int64) {
	var n atomic.Int64
	return func(context.Context) error {
		i := int(n.Add(1)) - 1
		if i < len(errs) {
			return errs[i]
		}
		return errs[len(errs)-1]
	}, &n
}

func TestDoSucceedsOnTheFirstTry(t *testing.T) {
	t.Parallel()
	fn, n := counting(nil)
	if err := newPolicy().Do(context.Background(), fn); err != nil || n.Load() != 1 {
		t.Fatalf("err=%v attempts=%d, want nil and 1", err, n.Load())
	}
}

func TestRetriesARetryableErrorUntilItSucceeds(t *testing.T) {
	t.Parallel()
	p := newPolicy()
	fn, n := counting(errRetry, errRetry, nil)
	if err := p.Do(context.Background(), fn); err != nil || n.Load() != 3 {
		t.Fatalf("err=%v attempts=%d, want success on the third", err, n.Load())
	}
	if s := p.Stats(); s.Calls != 1 || s.Attempts != 3 || s.Retries != 2 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestDoesNotRetryAnErrorThatIsNotRetryable(t *testing.T) {
	t.Parallel()
	fn, n := counting(errFatal)
	if err := newPolicy().Do(context.Background(), fn); !errors.Is(err, errFatal) || n.Load() != 1 {
		t.Fatalf("err=%v attempts=%d, want the error after one attempt", err, n.Load())
	}
}

func TestStopsAtMaxAttemptsAndReturnsTheLastError(t *testing.T) {
	t.Parallel()
	fn, n := counting(errRetry)
	err := newPolicy(resilience.WithMaxAttempts(4), resilience.WithoutBudget()).Do(context.Background(), fn)
	if !errors.Is(err, errRetry) || n.Load() != 4 {
		t.Fatalf("err=%v attempts=%d, want 4 attempts", err, n.Load())
	}
}

// The reason this package exists: during an outage the retries stay a bounded
// share of the traffic instead of multiplying it by MaxAttempts.
func TestBudgetCapsRetriesDuringAnOutage(t *testing.T) {
	t.Parallel()
	clk := clock.NewMock(time.Unix(0, 0))
	budget := resilience.NewBudget(0, 0, resilience.WithBudgetClock(clk), resilience.WithBurst(3))
	p := newPolicy(resilience.WithMaxAttempts(5), resilience.WithBudget(budget))

	fn, _ := counting(errRetry)
	for i := 0; i < 10; i++ {
		if err := p.Do(context.Background(), fn); !errors.Is(err, errRetry) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	s := p.Stats()
	if s.Retries != 3 {
		t.Fatalf("retries = %d, want exactly the burst of 3 across all ten calls (stats %+v)", s.Retries, s)
	}
	if s.Attempts != 10+3 {
		t.Fatalf("attempts = %d, want one per call plus 3 retries", s.Attempts)
	}
	if s.BudgetExhausted == 0 {
		t.Fatal("the budget never reported being exhausted")
	}
}

func TestBudgetRefillsWithTimeAndDeposits(t *testing.T) {
	t.Parallel()
	clk := clock.NewMock(time.Unix(0, 0))
	b := resilience.NewBudget(0.5, 2, resilience.WithBudgetClock(clk), resilience.WithBurst(4))
	for i := 0; i < 4; i++ {
		if !b.TryWithdraw() {
			t.Fatalf("withdrawal %d refused from a full bucket", i)
		}
	}
	if b.TryWithdraw() {
		t.Fatal("an empty bucket gave a token")
	}
	clk.Advance(time.Second) // 2 tokens a second
	for i := 0; i < 2; i++ {
		if !b.TryWithdraw() {
			t.Fatalf("a second of refill should give two tokens; withdrawal %d was refused (have %.2f)", i+1, b.Tokens())
		}
	}
	if b.TryWithdraw() {
		t.Fatalf("a second of refill gave more than two tokens (have %.2f)", b.Tokens())
	}
	b.Deposit()
	b.Deposit() // two calls at 0.5 each
	if !b.TryWithdraw() || b.TryWithdraw() {
		t.Fatalf("two deposits of 0.5 should give exactly one token (have %.2f)", b.Tokens())
	}
	clk.Advance(time.Hour)
	if got := b.Tokens(); got != 4 {
		t.Fatalf("the bucket should cap at its burst of 4, has %.2f", got)
	}
}

func TestNilBudgetIsUnlimited(t *testing.T) {
	t.Parallel()
	var b *resilience.Budget
	b.Deposit()
	if !b.TryWithdraw() {
		t.Fatal("a nil budget refused a retry")
	}
}

func TestBreakerOpensAndRefusesWithoutCallingTheUpstream(t *testing.T) {
	t.Parallel()
	br := circuitbreaker.New(circuitbreaker.Options{Upstream: "test", FailureThreshold: 2, OpenFor: time.Hour})
	p := newPolicy(resilience.WithMaxAttempts(1), resilience.WithBreaker(br))
	fn, n := counting(errRetry)

	for i := 0; i < 2; i++ {
		_ = p.Do(context.Background(), fn)
	}
	err := p.Do(context.Background(), fn)
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("third call = %v, want ErrCircuitOpen", err)
	}
	if n.Load() != 2 {
		t.Fatalf("the open breaker still let the upstream be called (%d calls)", n.Load())
	}
	if resilience.Retryable(err) {
		t.Fatal("a refusal by the breaker must not be retryable")
	}
}

func TestAnAnswerFromTheUpstreamDoesNotTripTheBreaker(t *testing.T) {
	t.Parallel()
	br := circuitbreaker.New(circuitbreaker.Options{Upstream: "test", FailureThreshold: 2, OpenFor: time.Hour})
	p := newPolicy(resilience.WithBreaker(br))
	fn, n := counting(errFatal) // e.g. a 404: the upstream is fine, the request is wrong
	for i := 0; i < 6; i++ {
		_ = p.Do(context.Background(), fn)
	}
	if br.State() != circuitbreaker.StateClosed || n.Load() != 6 {
		t.Fatalf("state=%v calls=%d: client errors must leave the breaker closed", br.State(), n.Load())
	}
}

func TestBreakerOpenedByEarlierAttemptsReportsThatFailure(t *testing.T) {
	t.Parallel()
	br := circuitbreaker.New(circuitbreaker.Options{Upstream: "test", FailureThreshold: 2, OpenFor: time.Hour})
	p := newPolicy(resilience.WithMaxAttempts(5), resilience.WithBreaker(br), resilience.WithoutBudget())
	fn, n := counting(errRetry)
	err := p.Do(context.Background(), fn)
	if !errors.Is(err, errRetry) || errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v, want the failure that opened the breaker, not a bare refusal", err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (the breaker opened after the second)", n.Load())
	}
}

func TestShedRefusesCallsOverTheLimit(t *testing.T) {
	t.Parallel()
	p := newPolicy(resilience.WithShed(loadshed.New("test", 1)))
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- p.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	var called atomic.Bool
	err := p.Do(context.Background(), func(context.Context) error { called.Store(true); return nil })
	if !errors.Is(err, resilience.ErrShed) || called.Load() {
		t.Fatalf("err=%v called=%v, want ErrShed without calling", err, called.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := p.Stats(); s.Shed != 1 {
		t.Fatalf("Shed = %d, want 1", s.Shed)
	}
	// The slot is free again.
	if err := p.Do(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("after the slot was released: %v", err)
	}
}

func TestCancellingTheContextEndsTheBackoffPromptly(t *testing.T) {
	t.Parallel()
	p := resilience.New("test", resilience.WithBackoff(retry.ConstantBackoff(time.Hour)), resilience.WithoutBudget())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	fn, n := counting(errRetry)
	start := time.Now()
	err := p.Do(ctx, fn)
	if !errors.Is(err, errRetry) || n.Load() != 1 {
		t.Fatalf("err=%v attempts=%d, want the first failure after one attempt", err, n.Load())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Do waited out the backoff instead of honouring the context")
	}
}

func TestConcurrentCallsAreSafe(t *testing.T) {
	t.Parallel()
	br := circuitbreaker.New(circuitbreaker.Options{Upstream: "test", FailureThreshold: 1000, OpenFor: time.Hour})
	p := newPolicy(resilience.WithBreaker(br), resilience.WithShed(loadshed.New("test", 8)))
	var wg sync.WaitGroup
	for g := 0; g < 40; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = p.Do(context.Background(), func(context.Context) error {
					if (g+i)%3 == 0 {
						return errRetry
					}
					return nil
				})
			}
		}(g)
	}
	wg.Wait()
	if s := p.Stats(); s.Calls != 40*50 {
		t.Fatalf("Calls = %d, want %d", s.Calls, 40*50)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetryable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                       {nil, false},
		"cancelled":                 {context.Canceled, false},
		"wrapped cancelled":         {fmt.Errorf("x: %w", context.Canceled), false},
		"deadline exceeded":         {context.DeadlineExceeded, true},
		"own method true":           {flaky{retry: true}, true},
		"own method false":          {flaky{retry: false}, false},
		"own method wins":           {fmt.Errorf("wrapped: %w", flaky{retry: false}), false},
		"transient marker":          {retry.Transient(errors.New("x")), true},
		"fleet 503":                 {fleeterrors.New(503, "unavailable", "x"), true},
		"fleet 429":                 {fleeterrors.New(429, "rate_limited", "x"), true},
		"fleet 404":                 {fleeterrors.New(404, "not_found", "x"), false},
		"fleet 400":                 {fleeterrors.New(400, "bad_request", "x"), false},
		"fleet 501":                 {fleeterrors.New(501, "not_implemented", "x"), false},
		"fleet unset status is 500": {&fleeterrors.Error{Code: "internal"}, true},
		"net timeout":               {timeoutErr{}, true},
		"refused connection":        {&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		"unexpected EOF":            {io.ErrUnexpectedEOF, true},
		"plain error":               {errors.New("who knows"), false},
	} {
		if got := resilience.Retryable(tc.err); got != tc.want {
			t.Errorf("%s: Retryable = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRetryableStatus(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]bool{
		200: false, 301: false, 400: false, 401: false, 404: false, 408: true, 409: false, 425: true,
		429: true, 500: true, 501: false, 502: true, 503: true, 504: true, 505: false, 599: true,
	} {
		if got := resilience.RetryableStatus(code); got != want {
			t.Errorf("RetryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
	_ = http.StatusOK
}

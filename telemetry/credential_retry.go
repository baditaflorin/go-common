package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/secrets"
)

// Init reads the OpenObserve ingestion credential once, synchronously, with a
// 2 s limit. A saturated host can exceed that (a direct read measured 2.1 s
// under load), and without a retry tracing then stays off until the next
// restart. When the failure is transient the read is therefore repeated in the
// background, bounded as follows:
//
//   - at most credentialRetryAttempts further attempts, after waits of 2, 4, 8,
//     16, 32 and 64 s (about two minutes), each wait shortened by up to
//     credentialRetryJitter so a fleet restarting together does not meet the
//     vault in lockstep;
//   - every attempt is the same read as the synchronous one: the same validated
//     vault URL and API key, the same direct and redirect-free client, the same
//     2 s limit; a success goes through the same credential, endpoint and
//     provider code as a start-time success;
//   - only transient failures are retried (see isTransientCredentialError). A
//     401, 403 or 404, or any other answer the vault gave on purpose, is a
//     configuration error: it is reported once and stays fail-closed;
//   - Config.Shutdown cancels the loop and waits for it, so nothing is
//     installed after Shutdown returns.
const (
	credentialRetryAttempts  = 6
	credentialRetryBaseDelay = 2 * time.Second
	credentialRetryMaxDelay  = 64 * time.Second
	credentialRetryJitter    = 0.2
)

// retryPolicy bounds and paces the background credential retry. The zero value
// is not usable; defaultRetryPolicy is what Init runs with.
type retryPolicy struct {
	attempts int
	base     time.Duration
	max      time.Duration
	// jitter returns a value in [0,1); each wait is shortened by that fraction
	// of credentialRetryJitter. Nil disables jitter.
	jitter func() float64
	// wait blocks for d or until ctx is done, in which case it returns ctx.Err().
	wait func(ctx context.Context, d time.Duration) error
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		attempts: credentialRetryAttempts,
		base:     credentialRetryBaseDelay,
		max:      credentialRetryMaxDelay,
		jitter:   rand.Float64,
		wait:     sleepContext,
	}
}

// delay is the wait before retry number n (1-based): base doubled for every
// earlier retry, capped at max, then shortened by the jitter.
func (p retryPolicy) delay(n int) time.Duration {
	d := p.base
	for i := 1; i < n && d < p.max; i++ {
		d *= 2
	}
	if d > p.max {
		d = p.max
	}
	if p.jitter != nil {
		d -= time.Duration(float64(d) * credentialRetryJitter * p.jitter())
	}
	return d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isTransientCredentialError reports whether a failed vault read may succeed
// if repeated shortly: the request timed out, the connection failed or dropped
// (any failure to obtain a response), or the vault or its gateway answered
// 5xx, 408 or 429. Everything else is a configuration error that retrying
// cannot fix and must keep failing closed: 401, 403, 404 and every other
// non-200 status, redirects (never followed), an empty value, an error
// envelope, a body that is not the expected envelope, and cancellation.
func isTransientCredentialError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var status *secrets.StatusError
	if errors.As(err, &status) {
		code := status.StatusCode
		return code >= 500 && code < 600 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// *url.Error, *net.OpError and *net.DNSError all satisfy net.Error: a
	// request that produced no response at all.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// credentialFailureReason names a failed vault read in a fixed vocabulary that
// is safe to log. It never includes error text, which can carry URLs.
func credentialFailureReason(err error) string {
	var status *secrets.StatusError
	var netErr net.Error
	switch {
	case err == nil:
		return "empty credential"
	case errors.As(err, &status):
		return fmt.Sprintf("HTTP %d", status.StatusCode)
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "connection dropped"
	case errors.As(err, &netErr):
		return "connection error"
	default:
		return "unusable response"
	}
}

// credentialAttempt wraps the vault read of one attempt so the retry can
// repeat exactly that read: the URL and key that passed the approved-host
// checks, without consulting the environment or key files again (a service may
// have removed either since start).
type credentialAttempt struct {
	read    func(ctx context.Context, baseURL, apiKey string) (string, error)
	baseURL string
	apiKey  string
	called  bool
	err     error
}

func (a *credentialAttempt) readToken(ctx context.Context, baseURL, apiKey string) (string, error) {
	a.baseURL, a.apiKey, a.called = baseURL, apiKey, true
	if a.read == nil {
		a.err = errors.New("telemetry: fleet secrets reader is unavailable")
		return "", a.err
	}
	token, err := a.read(ctx, baseURL, apiKey)
	a.err = err
	return token, err
}

// transient reports whether the attempt reached the vault read and failed in a
// way a later attempt may cure. A failure before the read (an unsafe key file,
// an unapproved vault URL) is never transient.
func (a *credentialAttempt) transient() bool {
	return a.called && isTransientCredentialError(a.err)
}

// credentialRetry is the background loop started by an Init whose credential
// read failed transiently. At most one runs per process; later Init calls that
// fail the same way share it.
type credentialRetry struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the goroutine has exited
}

// deferToCredentialRetry leaves tracing off for now and arranges for the
// background retry to install it. A credential-less provider is deliberately
// not installed in the meantime: it could never export, and the shared
// provider slot must stay free for the real one.
func deferToCredentialRetry(deps initDeps, cfg *Config, attempt credentialAttempt) *Config {
	providerMu.Lock()
	defer providerMu.Unlock()
	if sharedProvider != nil {
		// An earlier Init already brought tracing up: share that provider.
		cfg.provider = sharedProvider
		return cfg
	}
	if pendingRetry == nil {
		pendingRetry = startCredentialRetry(deps, cfg, attempt)
	}
	cfg.retry = pendingRetry
	return cfg
}

func startCredentialRetry(deps initDeps, cfg *Config, attempt credentialAttempt) *credentialRetry {
	ctx, cancel := context.WithCancel(context.Background())
	r := &credentialRetry{cancel: cancel, done: make(chan struct{})}
	// The goroutine works on its own copy of the settings: the *Config handed
	// to the caller is never touched once Init has returned.
	base := Config{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRate:     cfg.SampleRate,
	}
	go r.run(ctx, deps, base, attempt)
	return r
}

// stop cancels the loop, aborting an attempt in flight, and waits for the
// goroutine to exit or ctx to end. Once it has returned nil, the loop cannot
// install a provider any more.
func (r *credentialRetry) stop(ctx context.Context) error {
	r.cancel()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release makes room for a later Init to start a fresh retry.
func (r *credentialRetry) release() {
	providerMu.Lock()
	defer providerMu.Unlock()
	if pendingRetry == r {
		pendingRetry = nil
	}
}

// run reports exactly one outcome: a log line when tracing comes up late, an
// error line when it gives up (bounded attempts used up, or a failure that is
// not transient), and nothing when cancelled by Shutdown or made redundant by
// another Init.
func (r *credentialRetry) run(ctx context.Context, deps initDeps, base Config, attempt credentialAttempt) {
	defer close(r.done)
	defer r.release()

	policy, started := deps.retry, time.Now()
	last := attempt.err
	for retry := 1; retry <= policy.attempts; retry++ {
		if policy.wait(ctx, policy.delay(retry)) != nil || ctx.Err() != nil {
			return
		}
		if tracingIsUp() {
			return
		}
		token, err := attempt.read(ctx, attempt.baseURL, attempt.apiKey)
		if ctx.Err() != nil {
			return
		}
		if err == nil && token != "" {
			installed, installErr := installLateProvider(ctx, deps, base, token)
			switch {
			case installErr != nil:
				deps.handle(installErr)
			case installed:
				deps.logf("telemetry: OpenObserve credential loaded on retry %d of %d, %s after startup; tracing is now enabled",
					retry, policy.attempts, time.Since(started).Round(time.Second))
			}
			return
		}
		last = err
		if !isTransientCredentialError(err) {
			deps.handle(fmt.Errorf("telemetry: OpenObserve credential read failed with a non-retryable error on retry %d (%s); tracing stays off until restart",
				retry, credentialFailureReason(err)))
			return
		}
	}
	deps.handle(fmt.Errorf("telemetry: OpenObserve credential still unavailable after %d retries over %s (last failure: %s); tracing stays off until restart",
		policy.attempts, time.Since(started).Round(time.Second), credentialFailureReason(last)))
}

func tracingIsUp() bool {
	providerMu.Lock()
	defer providerMu.Unlock()
	return sharedProvider != nil
}

// installLateProvider turns a credential obtained on retry into the shared
// provider, through the same applyOpenObserveToken and newProvider a
// start-time success uses. It reports installed=false without an error when it
// stood aside: Shutdown has been requested, or another Init got there first.
func installLateProvider(ctx context.Context, deps initDeps, base Config, token string) (installed bool, err error) {
	cfg := base
	if !openObserveCredentialAllowed(cfg.OTLPEndpoint) {
		// The synchronous attempt already enforced this before reading the
		// credential. Repeat it where the credential is attached, so the late
		// path can never widen where the token is sent.
		return false, errors.New("telemetry: OpenObserve ingestion credential is only used with the approved OTLP hosts")
	}
	applyOpenObserveToken(&cfg, token)

	providerMu.Lock()
	defer providerMu.Unlock()
	// Shutdown cancels ctx and then waits for this goroutine, so checking under
	// the lock that installs the provider means a Shutdown that has returned
	// can never be followed by an install.
	if ctx.Err() != nil || sharedProvider != nil {
		return false, nil
	}
	tp, err := deps.newProvider(&cfg)
	if err != nil {
		return false, err
	}
	sharedProvider = tp
	return true, nil
}

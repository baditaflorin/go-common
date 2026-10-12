package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/secrets"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	testVaultKey   = "test-vault-api-key"
	testVaultToken = "o2oi_late-test-token"
	// testWait only bounds events that must happen; no test sleeps for it.
	testWait = 5 * time.Second
)

func vaultEnv(key string) string {
	if key == "FLEET_SECRETS_API_KEY" {
		return testVaultKey
	}
	return ""
}

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// resetProviderState stops any retry a test left behind and empties the
// process-wide slots.
func resetProviderState() {
	providerMu.Lock()
	r := pendingRetry
	providerMu.Unlock()
	if r != nil {
		// Bounded, so a broken implementation fails a test rather than
		// hanging the whole run in cleanup.
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		_ = r.stop(ctx)
		cancel()
	}
	providerMu.Lock()
	sharedProvider, pendingRetry = nil, nil
	providerMu.Unlock()
}

// isolateProviderState gives a test an empty provider slot and restores what
// was there, so these tests neither depend on nor disturb any other.
func isolateProviderState(t *testing.T) {
	t.Helper()
	providerMu.Lock()
	prevProvider, prevRetry := sharedProvider, pendingRetry
	providerMu.Unlock()
	resetProviderState()
	t.Cleanup(func() {
		resetProviderState()
		providerMu.Lock()
		sharedProvider, pendingRetry = prevProvider, prevRetry
		providerMu.Unlock()
	})
}

func installedProvider() *sdktrace.TracerProvider {
	providerMu.Lock()
	defer providerMu.Unlock()
	return sharedProvider
}

func pendingRetryNow() *credentialRetry {
	providerMu.Lock()
	defer providerMu.Unlock()
	return pendingRetry
}

// countingProcessor lets a test see whether the provider it was attached to
// has been shut down, without any exporter or network behind it.
type countingProcessor struct{ shutdowns atomic.Int32 }

func (*countingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (*countingProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (*countingProcessor) ForceFlush(context.Context) error                { return nil }
func (p *countingProcessor) Shutdown(context.Context) error {
	p.shutdowns.Add(1)
	return nil
}

func newCountedProvider(p *countingProcessor) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p))
}

// sink collects log or error lines and lets a test wait for the next one.
type sink struct {
	mu    sync.Mutex
	lines []string
	added chan struct{}
}

func newSink() *sink { return &sink{added: make(chan struct{}, 64)} }

func (s *sink) add(line string) {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	select {
	case s.added <- struct{}{}:
	default:
	}
}

func (s *sink) logf(format string, args ...any) { s.add(fmt.Sprintf(format, args...)) }
func (s *sink) handle(err error)                { s.add(err.Error()) }

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *sink) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(testWait)
	for len(s.all()) < n {
		select {
		case <-s.added:
		case <-deadline:
			t.Fatalf("timed out waiting for %d line(s); have %q", n, s.all())
		}
	}
}

// waits stands in for the backoff sleep: it records the requested delays and
// returns at once, so the real schedule can be asserted in no time.
type waits struct {
	mu sync.Mutex
	ds []time.Duration
}

func (w *waits) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.ds = append(w.ds, d)
	w.mu.Unlock()
	return ctx.Err()
}

func (w *waits) all() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.ds...)
}

// gatedFirstWait holds the first backoff until the test opens the gate, so a
// test can inspect the state between Init returning and the first retry.
func gatedFirstWait(gate <-chan struct{}) func(context.Context, time.Duration) error {
	var calls atomic.Int32
	return func(ctx context.Context, d time.Duration) error {
		if calls.Add(1) == 1 {
			select {
			case <-gate:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return sleepContext(ctx, d)
	}
}

// fastPolicy keeps the production attempt bound with millisecond waits.
func fastPolicy() retryPolicy {
	return retryPolicy{attempts: credentialRetryAttempts, base: time.Millisecond, max: 4 * time.Millisecond, wait: sleepContext}
}

// harness is a fake Fleet Secrets vault behind the production secrets client,
// plus fakes for everything else Init touches.
type harness struct {
	t      *testing.T
	vault  *httptest.Server
	client *http.Client // mirrors newOpenObserveTokenHTTPClient, with a test-sized limit
	seen   atomic.Int64 // requests that reached the vault
	logs   *sink        // informational lines
	errs   *sink        // error lines reported through the OTel error handler
	proc   *countingProcessor
	deps   initDeps

	mu    sync.Mutex
	calls [][2]string // baseURL, apiKey of every read the production code asked for
	built []Config    // configuration of every provider the code asked to create
}

func newHarness(t *testing.T, behave func(n int64, w http.ResponseWriter, r *http.Request), policy retryPolicy) *harness {
	t.Helper()
	isolateProviderState(t)
	h := &harness{t: t, logs: newSink(), errs: newSink(), proc: &countingProcessor{}}
	// Production builds a fresh transport for every attempt, so one attempt is
	// one request on a new connection. Keep-alive reuse would let net/http
	// silently re-send a GET after a dropped connection, behind the retry
	// loop's back, and make the request counts below meaningless.
	transport := secrets.DirectTransport()
	transport.DisableKeepAlives = true
	h.client = &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     transport,
	}
	h.vault = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := h.seen.Add(1)
		if r.URL.Path != "/secrets/"+openObserveTokenSecret || r.Header.Get("X-API-Key") != testVaultKey {
			t.Errorf("unexpected vault request: path=%q keyOK=%v", r.URL.Path, r.Header.Get("X-API-Key") == testVaultKey)
		}
		behave(n, w, r)
	}))
	t.Cleanup(func() {
		h.client.CloseIdleConnections()
		h.vault.Close()
	})
	h.deps = initDeps{
		getenv:    vaultEnv,
		readToken: h.readToken,
		newProvider: func(cfg *Config) (*sdktrace.TracerProvider, error) {
			h.mu.Lock()
			h.built = append(h.built, *cfg)
			h.mu.Unlock()
			return newCountedProvider(h.proc), nil
		},
		retry:  policy,
		logf:   h.logs.logf,
		handle: h.errs.handle,
	}
	return h
}

func (h *harness) readToken(ctx context.Context, baseURL, apiKey string) (string, error) {
	h.mu.Lock()
	h.calls = append(h.calls, [2]string{baseURL, apiKey})
	h.mu.Unlock()
	return secrets.New(h.vault.URL, apiKey, h.client).Get(ctx, openObserveTokenSecret)
}

func (h *harness) builtConfigs() []Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Config(nil), h.built...)
}

func (h *harness) readCalls() [][2]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][2]string(nil), h.calls...)
}

// requireEveryReadUsedTheValidatedInputs asserts that the synchronous attempt
// and every retry went to the approved vault URL with the configured key.
func (h *harness) requireEveryReadUsedTheValidatedInputs(want int) {
	h.t.Helper()
	calls := h.readCalls()
	if len(calls) != want {
		h.t.Fatalf("vault reads = %d, want %d", len(calls), want)
	}
	for i, call := range calls {
		if call[0] != defaultFleetSecretsURL || call[1] != testVaultKey {
			h.t.Fatalf("read %d used url=%q keyOK=%v; every attempt must reuse the approved URL and key", i+1, call[0], call[1] == testVaultKey)
		}
	}
}

// requireNoSecretsInOutput asserts that neither the credential nor the vault
// API key (nor the Authorization value derived from it) reached any line.
func (h *harness) requireNoSecretsInOutput() {
	h.t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte("default:" + testVaultToken))
	for _, line := range append(h.logs.all(), h.errs.all()...) {
		for _, secret := range []string{testVaultToken, testVaultKey, basic, "Authorization"} {
			if strings.Contains(line, secret) {
				h.t.Fatalf("output leaked a secret (%q): %q", secret, line)
			}
		}
	}
}

func writeVaultToken(w http.ResponseWriter, token string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"success","data":{"name":%q,"value":%q}}`, openObserveTokenSecret, token)
}

func dropConnection(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// hang holds the request until the client gives up (limit or cancellation).
func hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(testWait):
	}
}

func replyStatus(code int) func(int64, http.ResponseWriter, *http.Request) {
	return func(_ int64, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func awaitDone(t *testing.T, r *credentialRetry) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(testWait):
		t.Fatal("the credential retry did not finish")
	}
}

// retryGoroutineRunning looks for the retry loop in a full goroutine profile.
// It is only used to confirm a goroutine is gone; a loop that is known to be
// pending is detected through the wait hook, never by scanning stacks.
func retryGoroutineRunning() bool {
	var dump strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&dump, 2)
	return strings.Contains(dump.String(), "(*credentialRetry).run")
}

func awaitNoRetryGoroutine(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for retryGoroutineRunning() {
		if time.Now().After(deadline) {
			t.Fatal("a credential retry goroutine is still running")
		}
		time.Sleep(time.Millisecond)
	}
}

const initialFailureLine = "telemetry: could not load OpenObserve ingestion credential from fleet secrets"

// The load that motivated the retry: one slow read at start must not leave
// tracing off for the life of the process.
func TestRetryBringsTracingUpAfterTransientFailures(t *testing.T) {
	gate := make(chan struct{})
	policy := fastPolicy()
	policy.wait = gatedFirstWait(gate)
	h := newHarness(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1: // the synchronous attempt: the gateway is overloaded
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2: // retry 1: the connection is reset
			dropConnection(w)
		case 3: // retry 2: slower than the per-attempt limit
			hang(r)
		default: // retry 3: healthy again
			writeVaultToken(w, testVaultToken)
		}
	}, policy)
	h.client.Timeout = 250 * time.Millisecond // only the hung request should ever hit this

	cfg := initWith(h.deps, "svc", "1.2.3")

	// Init returned without waiting: nothing installed, one retry pending, no
	// further vault traffic until the first backoff ends.
	if cfg.provider != nil || cfg.retry == nil || len(h.builtConfigs()) != 0 || h.seen.Load() != 1 {
		t.Fatalf("after Init: provider=%v retry=%v built=%d requests=%d", cfg.provider != nil, cfg.retry != nil, len(h.builtConfigs()), h.seen.Load())
	}
	if pendingRetryNow() != cfg.retry {
		t.Fatal("the pending retry is not the one Init handed to the config")
	}
	if got := h.errs.all(); len(got) != 1 || got[0] != initialFailureLine {
		t.Fatalf("the start-time failure must still be reported exactly once: %q", got)
	}

	close(gate)
	h.logs.await(t, 1)
	awaitDone(t, cfg.retry)

	if got := h.seen.Load(); got != 4 {
		t.Fatalf("vault requests = %d, want 4 (start-time attempt, two failed retries, one success)", got)
	}
	h.requireEveryReadUsedTheValidatedInputs(4)
	logs := h.logs.all()
	if len(logs) != 1 || !strings.Contains(logs[0], "on retry 3 of 6") || !strings.Contains(logs[0], "tracing is now enabled") {
		t.Fatalf("want one 'came up late' line, got %q", logs)
	}
	if got := h.errs.all(); len(got) != 1 {
		t.Fatalf("a recovered retry must add no error line: %q", got)
	}

	built := h.builtConfigs()
	if len(built) != 1 {
		t.Fatalf("providers created = %d, want exactly 1", len(built))
	}
	got := built[0]
	if got.OTLPEndpoint != defaultOpenObserveEndpoint || got.ServiceName != "svc" || got.ServiceVersion != "1.2.3" || got.SampleRate != 0.1 {
		t.Fatalf("late provider configured as %+v", got)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("default:"+testVaultToken))
	if got.otlpHeaders["Authorization"] != wantAuth || got.otlpHeaders["stream-name"] != "default" {
		t.Fatal("the late provider did not get the scoped OpenObserve credential")
	}
	if installedProvider() == nil || pendingRetryNow() != nil {
		t.Fatal("after recovery the provider must be installed and no retry pending")
	}

	if h.proc.shutdowns.Load() != 0 {
		t.Fatal("provider shut down before Shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if h.proc.shutdowns.Load() != 1 {
		t.Fatal("Shutdown must flush and close the provider the retry installed")
	}
	h.requireNoSecretsInOutput()
	awaitNoRetryGoroutine(t)
}

func TestRetryGivesUpAfterTheBoundedAttempts(t *testing.T) {
	recorded := &waits{}
	policy := retryPolicy{attempts: credentialRetryAttempts, base: credentialRetryBaseDelay, max: credentialRetryMaxDelay, wait: recorded.wait}
	h := newHarness(t, replyStatus(http.StatusServiceUnavailable), policy)

	cfg := initWith(h.deps, "svc", "1.2.3")
	if cfg.retry == nil {
		t.Fatal("a 503 on the synchronous attempt must start a retry")
	}
	awaitDone(t, cfg.retry)

	if got := h.seen.Load(); got != 1+credentialRetryAttempts {
		t.Fatalf("vault requests = %d, want %d (start-time attempt plus %d retries)", got, 1+credentialRetryAttempts, credentialRetryAttempts)
	}
	h.requireEveryReadUsedTheValidatedInputs(1 + credentialRetryAttempts)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second}
	if got := recorded.all(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("backoff schedule = %v, want %v", got, want)
	}

	errs := h.errs.all()
	if len(errs) != 2 || errs[0] != initialFailureLine {
		t.Fatalf("want the start-time failure plus one give-up line, got %q", errs)
	}
	for _, part := range []string{"still unavailable after 6 retries", "last failure: HTTP 503", "tracing stays off until restart"} {
		if !strings.Contains(errs[1], part) {
			t.Fatalf("give-up line %q lacks %q", errs[1], part)
		}
	}
	if len(h.logs.all()) != 0 || len(h.builtConfigs()) != 0 || installedProvider() != nil {
		t.Fatal("a vault that never recovers must leave tracing off and the 'came up late' line unwritten")
	}
	if pendingRetryNow() != nil {
		t.Fatal("an exhausted retry must release the slot")
	}
	h.requireNoSecretsInOutput()

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after exhaustion: %v", err)
	}
	if got := h.seen.Load(); got != 1+credentialRetryAttempts {
		t.Fatalf("requests changed after exhaustion: %d", got)
	}
	awaitNoRetryGoroutine(t)
}

// 401, 403 and 404 are configuration errors. They stay fail-closed: the start
// time failure is reported once and nothing is scheduled, however often a
// retry could have run.
func TestPermanentVaultAnswersAreNeverRetried(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusFound} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			recorded := &waits{}
			policy := fastPolicy()
			policy.wait = recorded.wait
			h := newHarness(t, func(_ int64, w http.ResponseWriter, _ *http.Request) {
				if code == http.StatusFound {
					w.Header().Set("Location", "https://attacker.invalid/steal")
				}
				w.WriteHeader(code)
			}, policy)

			cfg := initWith(h.deps, "svc", "1.2.3")

			if cfg.retry != nil || pendingRetryNow() != nil {
				t.Fatalf("HTTP %d started a retry", code)
			}
			if got := recorded.all(); len(got) != 0 {
				t.Fatalf("HTTP %d scheduled backoff waits: %v", code, got)
			}
			time.Sleep(20 * time.Millisecond) // six 1-4 ms retries would all have fired by now
			if got := h.seen.Load(); got != 1 {
				t.Fatalf("vault requests = %d, want exactly 1 for HTTP %d", got, code)
			}
			if got := h.errs.all(); len(got) != 1 || got[0] != initialFailureLine {
				t.Fatalf("want the start-time failure once, got %q", got)
			}
			if len(h.logs.all()) != 0 || len(h.builtConfigs()) != 0 || installedProvider() != nil {
				t.Fatal("a configuration error must leave tracing off")
			}
			awaitNoRetryGoroutine(t)
		})
	}
}

// Every other late-install test substitutes the provider constructor. This
// one runs the production newProvider: a real batching provider for the
// approved default endpoint, installed as the process's global tracer provider
// only after the retry succeeds. It samples nothing, so no span exists and no
// network traffic can result.
func TestLateInstallBuildsTheRealProviderAndShutsItDownCleanly(t *testing.T) {
	for _, key := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY", "OTEL_EXPORTER_OTLP_INSECURE"} {
		t.Setenv(key, "")
	}
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	gate := make(chan struct{})
	policy := fastPolicy()
	policy.wait = gatedFirstWait(gate)
	h := newHarness(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeVaultToken(w, testVaultToken)
	}, policy)
	h.deps.newProvider = newProvider // the production constructor
	h.deps.getenv = envFrom(map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "OTEL_SAMPLE_RATE": "0"})

	cfg := initWith(h.deps, "svc", "1.2.3")
	if installedProvider() != nil || otel.GetTracerProvider() != previous {
		t.Fatal("nothing may be installed before the credential has been read")
	}

	close(gate)
	h.logs.await(t, 1)
	awaitDone(t, cfg.retry)

	tp := installedProvider()
	if tp == nil || otel.GetTracerProvider() != trace.TracerProvider(tp) {
		t.Fatal("the late provider must be the shared provider and the global tracer provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown of the real late provider: %v", err)
	}
	h.requireNoSecretsInOutput()
}

// A retry can also meet a permanent answer (a key revoked mid-outage): it
// stops at once and says so.
func TestRetryStopsWhenTheFailureBecomesPermanent(t *testing.T) {
	h := newHarness(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}, fastPolicy())

	cfg := initWith(h.deps, "svc", "1.2.3")
	awaitDone(t, cfg.retry)

	if got := h.seen.Load(); got != 2 {
		t.Fatalf("vault requests = %d, want 2: a 403 must end the retry", got)
	}
	errs := h.errs.all()
	if len(errs) != 2 || !strings.Contains(errs[1], "non-retryable error on retry 1 (HTTP 403)") || !strings.Contains(errs[1], "tracing stays off until restart") {
		t.Fatalf("unexpected error lines: %q", errs)
	}
	if len(h.logs.all()) != 0 || installedProvider() != nil || pendingRetryNow() != nil {
		t.Fatal("a retry that hit a configuration error must leave tracing off and release the slot")
	}
}

func TestShutdownCancelsAPendingRetry(t *testing.T) {
	sleeping := make(chan struct{}, 1)
	policy := retryPolicy{attempts: credentialRetryAttempts, base: time.Hour, max: time.Hour, wait: func(ctx context.Context, d time.Duration) error {
		select {
		case sleeping <- struct{}{}:
		default:
		}
		return sleepContext(ctx, d)
	}}
	h := newHarness(t, replyStatus(http.StatusServiceUnavailable), policy)

	cfg := initWith(h.deps, "svc", "1.2.3")
	if cfg.retry == nil {
		t.Fatal("a 503 on the synchronous attempt must start a retry")
	}
	select {
	case <-sleeping: // the loop is now inside its hour-long first backoff
	case <-time.After(testWait):
		t.Fatal("the retry never entered its first backoff")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	begun := time.Now()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if took := time.Since(begun); took > time.Second {
		t.Fatalf("Shutdown took %v; it must cut the backoff short", took)
	}
	select {
	case <-cfg.retry.done:
	default:
		t.Fatal("Shutdown returned while the retry goroutine was still running")
	}
	if pendingRetryNow() != nil || installedProvider() != nil || len(h.builtConfigs()) != 0 {
		t.Fatal("a cancelled retry must leave no provider and no pending slot")
	}
	if got := h.seen.Load(); got != 1 {
		t.Fatalf("vault requests = %d; a cancelled retry must not read again", got)
	}
	if len(h.logs.all()) != 0 || len(h.errs.all()) != 1 {
		t.Fatalf("cancellation must be silent: logs=%q errs=%q", h.logs.all(), h.errs.all())
	}
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("a second Shutdown must be harmless: %v", err)
	}
	awaitNoRetryGoroutine(t)
}

func TestShutdownAbortsAnAttemptInFlight(t *testing.T) {
	inFlight, aborted := make(chan struct{}), make(chan struct{})
	h := newHarness(t, func(n int64, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		close(inFlight)
		<-r.Context().Done() // returns when the client abandons the request
		close(aborted)
	}, fastPolicy())
	h.client.Timeout = testWait // only cancellation can end this attempt early

	cfg := initWith(h.deps, "svc", "1.2.3")
	select {
	case <-inFlight:
	case <-time.After(testWait):
		t.Fatal("the retry never reached the vault")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	begun := time.Now()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if took := time.Since(begun); took > 2*time.Second {
		t.Fatalf("Shutdown took %v; it must abort the request, not wait out the limit", took)
	}
	select {
	case <-aborted:
	case <-time.After(testWait):
		t.Fatal("the in-flight vault request was not cancelled")
	}
	if installedProvider() != nil || len(h.builtConfigs()) != 0 || pendingRetryNow() != nil {
		t.Fatal("an aborted attempt must install nothing")
	}
	if len(h.logs.all()) != 0 || len(h.errs.all()) != 1 {
		t.Fatalf("cancellation must be silent: logs=%q errs=%q", h.logs.all(), h.errs.all())
	}
	awaitNoRetryGoroutine(t)
}

// The invariant that makes Shutdown clean: once it has returned, nothing can
// install a provider, and a provider installed before it was shut down. Run
// under -race this also exercises the unsynchronised-looking hand-off between
// the retry goroutine and Shutdown.
func TestShutdownRacingALateInstallLeavesNothingBehind(t *testing.T) {
	iterations := 300
	if testing.Short() {
		iterations = 40
	}
	t.Cleanup(resetProviderState)
	for i := 0; i < iterations; i++ {
		resetProviderState()
		proc, installs, reads := &countingProcessor{}, &atomic.Int32{}, &atomic.Int32{}
		deps := initDeps{
			getenv: vaultEnv,
			readToken: func(context.Context, string, string) (string, error) {
				if reads.Add(1) == 1 {
					return "", context.DeadlineExceeded
				}
				return testVaultToken, nil
			},
			newProvider: func(*Config) (*sdktrace.TracerProvider, error) {
				installs.Add(1)
				return newCountedProvider(proc), nil
			},
			retry:  retryPolicy{attempts: credentialRetryAttempts, wait: sleepContext}, // zero delays: the retry races Shutdown
			logf:   func(string, ...any) {},
			handle: func(error) {},
		}

		cfg := initWith(deps, "svc", "1.2.3")
		for spin := 0; spin < i%7; spin++ {
			runtime.Gosched()
		}
		if i%3 == 0 {
			time.Sleep(time.Duration(i%50) * time.Microsecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		err := cfg.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: Shutdown: %v", i, err)
		}

		select {
		case <-cfg.retry.done:
		default:
			t.Fatalf("iteration %d: Shutdown returned with the retry still running", i)
		}
		installed := installs.Load()
		if installed > 1 {
			t.Fatalf("iteration %d: %d providers installed", i, installed)
		}
		if closed := proc.shutdowns.Load(); closed != installed {
			t.Fatalf("iteration %d: %d providers installed but %d shut down", i, installed, closed)
		}
		if up := installedProvider() != nil; up != (installed == 1) {
			t.Fatalf("iteration %d: shared provider present=%v, installs=%d", i, up, installed)
		}
		reads.Store(1 << 20) // any rogue straggler would now find a healthy vault
		time.Sleep(50 * time.Microsecond)
		if installs.Load() != installed {
			t.Fatalf("iteration %d: a provider was installed after Shutdown returned", i)
		}
	}
	awaitNoRetryGoroutine(t)
}

// One process shares one provider, and now one retry: two services built in
// one process must not double the vault traffic of an outage.
func TestSecondInitSharesTheRetryAndTheProvider(t *testing.T) {
	gate := make(chan struct{})
	policy := fastPolicy()
	policy.wait = gatedFirstWait(gate)
	h := newHarness(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // both synchronous attempts
			return
		}
		writeVaultToken(w, testVaultToken)
	}, policy)

	first := initWith(h.deps, "svc", "1.2.3")
	second := initWith(h.deps, "svc", "1.2.3")
	if first.retry == nil || first.retry != second.retry {
		t.Fatal("a second failing Init must share the pending retry, not start another")
	}

	close(gate)
	h.logs.await(t, 1)
	awaitDone(t, first.retry)
	if got := h.seen.Load(); got != 3 {
		t.Fatalf("vault requests = %d, want 3 (two start-time attempts, one retry)", got)
	}
	if got := len(h.builtConfigs()); got != 1 {
		t.Fatalf("providers created = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := second.Shutdown(ctx); err != nil {
		t.Fatalf("second.Shutdown: %v", err)
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("first.Shutdown: %v", err)
	}
	if got := h.proc.shutdowns.Load(); got != 1 {
		t.Fatalf("shared provider shut down %d times, want once", got)
	}
}

func TestInitSharesAnExistingProviderInsteadOfRetrying(t *testing.T) {
	h := newHarness(t, replyStatus(http.StatusServiceUnavailable), fastPolicy())
	existing := &countingProcessor{}
	providerMu.Lock()
	sharedProvider = newCountedProvider(existing)
	providerMu.Unlock()

	cfg := initWith(h.deps, "svc", "1.2.3")
	if cfg.retry != nil || pendingRetryNow() != nil {
		t.Fatal("no retry is needed when tracing is already up in this process")
	}
	if cfg.provider != installedProvider() {
		t.Fatal("the config must share the provider that is already installed")
	}
	time.Sleep(20 * time.Millisecond)
	if got := h.seen.Load(); got != 1 {
		t.Fatalf("vault requests = %d, want 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := cfg.Shutdown(ctx); err != nil || existing.shutdowns.Load() != 1 {
		t.Fatalf("Shutdown err=%v shutdowns=%d", err, existing.shutdowns.Load())
	}
}

// An earlier Init whose retry is still waiting must stand down, without
// another vault read, once a later Init has brought tracing up.
func TestRetryStandsDownWhenAnotherInitBringsTracingUp(t *testing.T) {
	gate := make(chan struct{})
	policy := fastPolicy()
	policy.wait = gatedFirstWait(gate)
	h := newHarness(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeVaultToken(w, testVaultToken)
	}, policy)

	first := initWith(h.deps, "svc", "1.2.3")
	if first.retry == nil {
		t.Fatal("first Init should be waiting on a retry")
	}
	second := initWith(h.deps, "svc", "1.2.3") // its own synchronous read succeeds
	if second.provider == nil || second.retry != nil || len(h.builtConfigs()) != 1 {
		t.Fatalf("second Init: provider=%v retry=%v providers=%d", second.provider != nil, second.retry != nil, len(h.builtConfigs()))
	}

	close(gate)
	awaitDone(t, first.retry)
	if got := h.seen.Load(); got != 2 {
		t.Fatalf("vault requests = %d, want 2: the retry must stand down without reading", got)
	}
	if len(h.logs.all()) != 0 || len(h.errs.all()) != 1 || len(h.builtConfigs()) != 1 {
		t.Fatalf("standing down must be silent and install nothing: logs=%q errs=%q providers=%d", h.logs.all(), h.errs.all(), len(h.builtConfigs()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("first.Shutdown: %v", err)
	}
	if got := h.proc.shutdowns.Load(); got != 1 {
		t.Fatalf("shared provider shut down %d times, want once", got)
	}
}

// Every guard that stops the vault read also stops the retry, because no
// attempt ever happens: approved-host checks, explicit headers, a collector
// that must never receive the token, an unsafe key file.
func TestNoVaultReadOrRetryWhenAConfigurationCheckFails(t *testing.T) {
	unsafeKeyFile := filepath.Join(t.TempDir(), "vault-key")
	if err := os.WriteFile(unsafeKeyFile, []byte("not-a-real-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		env      map[string]string
		wantErrs int
	}{
		{"no vault credential configured", map[string]string{}, 0},
		{"cleartext vault URL", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "FLEET_SECRETS_URL": "http://fleet-secrets.0exec.com"}, 1},
		{"unapproved vault host", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "FLEET_SECRETS_URL": "https://attacker.example"}, 1},
		{"vault URL with credentials", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "FLEET_SECRETS_URL": "https://user:pw@fleet-secrets.0exec.com"}, 1},
		{"explicit OTLP headers", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer explicit"}, 0},
		{"unapproved collector endpoint", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/v1/traces"}, 0},
		{"tracing disabled", map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "OTEL_DISABLED": "true"}, 0},
		{"key file with unsafe permissions", map[string]string{"FLEET_SECRETS_API_KEY_FILE": unsafeKeyFile}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorded := &waits{}
			policy := fastPolicy()
			policy.wait = recorded.wait
			h := newHarness(t, replyStatus(http.StatusServiceUnavailable), policy)
			h.deps.getenv = envFrom(tc.env)

			cfg := initWith(h.deps, "svc", "1.2.3")

			if cfg.retry != nil || pendingRetryNow() != nil || len(recorded.all()) != 0 {
				t.Fatal("a retry was scheduled although no vault read was ever allowed")
			}
			time.Sleep(10 * time.Millisecond)
			if h.seen.Load() != 0 || len(h.readCalls()) != 0 {
				t.Fatalf("the vault was contacted: requests=%d reads=%d", h.seen.Load(), len(h.readCalls()))
			}
			if got := h.errs.all(); len(got) != tc.wantErrs {
				t.Fatalf("error lines = %q, want %d", got, tc.wantErrs)
			}
			awaitNoRetryGoroutine(t)
		})
	}
}

// An operator-pinned approved endpoint must not get a provider that exports
// without a credential while the retry is still working: it could never
// authenticate, and it would occupy the one shared provider slot.
func TestExplicitApprovedEndpointWaitsForTheCredentialInsteadOfExportingWithout(t *testing.T) {
	const endpoint = "https://otlp.0exec.com/api/default/v1/traces"
	gate := make(chan struct{})
	policy := fastPolicy()
	policy.wait = gatedFirstWait(gate)
	h := newHarness(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeVaultToken(w, testVaultToken)
	}, policy)
	h.deps.getenv = envFrom(map[string]string{"FLEET_SECRETS_API_KEY": testVaultKey, "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint})

	cfg := initWith(h.deps, "svc", "1.2.3")
	if cfg.retry == nil || len(h.builtConfigs()) != 0 || installedProvider() != nil {
		t.Fatalf("while the credential is pending: retry=%v providers=%d", cfg.retry != nil, len(h.builtConfigs()))
	}

	close(gate)
	h.logs.await(t, 1)
	awaitDone(t, cfg.retry)
	built := h.builtConfigs()
	if len(built) != 1 || built[0].OTLPEndpoint != endpoint || built[0].otlpHeaders["Authorization"] == "" {
		t.Fatalf("the late provider must keep the pinned endpoint and carry the credential: %+v", built)
	}
}

// The late path re-checks, where the credential is attached, that the endpoint
// is one the credential may go to. Init's own check makes this unreachable
// today; the guard keeps it that way if either side changes.
func TestLateInstallRefusesAnUnapprovedEndpoint(t *testing.T) {
	isolateProviderState(t)
	var built atomic.Int32
	deps := initDeps{newProvider: func(*Config) (*sdktrace.TracerProvider, error) {
		built.Add(1)
		return nil, errors.New("must not be called")
	}}
	installed, err := installLateProvider(context.Background(), deps, Config{ServiceName: "svc", OTLPEndpoint: "https://collector.example/v1/traces"}, testVaultToken)
	if installed || err == nil || built.Load() != 0 || installedProvider() != nil {
		t.Fatalf("installed=%v err=%v providers=%d", installed, err, built.Load())
	}
	if strings.Contains(err.Error(), testVaultToken) {
		t.Fatal("the refusal leaked the credential")
	}
}

// Shutdown is given a bounded context. If it runs out while a read that
// ignores cancellation is still stuck, Shutdown reports that, and the retry
// still must not install anything once the read finally returns.
func TestShutdownThatTimesOutStillPreventsALateInstall(t *testing.T) {
	isolateProviderState(t)
	inRead, release := make(chan struct{}), make(chan struct{})
	var reads, installs atomic.Int32
	proc := &countingProcessor{}
	deps := initDeps{
		getenv: vaultEnv,
		readToken: func(context.Context, string, string) (string, error) {
			if reads.Add(1) == 1 {
				return "", context.DeadlineExceeded
			}
			close(inRead)
			<-release // a read that does not honour its context
			return testVaultToken, nil
		},
		newProvider: func(*Config) (*sdktrace.TracerProvider, error) {
			installs.Add(1)
			return newCountedProvider(proc), nil
		},
		retry:  fastPolicy(),
		logf:   func(string, ...any) {},
		handle: func(error) {},
	}

	cfg := initWith(deps, "svc", "1.2.3")
	select {
	case <-inRead:
	case <-time.After(testWait):
		t.Fatal("the retry never started its read")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cfg.Shutdown(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown with an exhausted context = %v, want context.Canceled", err)
	}

	close(release)
	awaitDone(t, cfg.retry)
	if installs.Load() != 0 || installedProvider() != nil || pendingRetryNow() != nil {
		t.Fatal("a provider was installed after Shutdown had been requested")
	}
}

func TestRetryScheduleIsBoundedToAboutTwoMinutes(t *testing.T) {
	policy := defaultRetryPolicy()
	if policy.attempts != 6 || policy.jitter == nil || policy.wait == nil {
		t.Fatalf("default policy = %+v", policy)
	}
	nominal := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second}
	var total time.Duration
	for i, want := range nominal {
		policy.jitter = nil
		if got := policy.delay(i + 1); got != want {
			t.Fatalf("delay(%d) = %v, want %v", i+1, got, want)
		}
		total += want
		policy.jitter = func() float64 { return 0.999999 }
		if got := policy.delay(i + 1); got < want*8/10 || got > want {
			t.Fatalf("delay(%d) with maximum jitter = %v, want within [80%%,100%%] of %v", i+1, got, want)
		}
		policy.jitter = func() float64 { return 0 }
		if got := policy.delay(i + 1); got != want {
			t.Fatalf("delay(%d) with no jitter draw = %v, want %v", i+1, got, want)
		}
	}
	if total != 126*time.Second {
		t.Fatalf("nominal total wait = %v, want 126s", total)
	}
	if worst := total + time.Duration(policy.attempts)*credentialReadTimeout; worst > 140*time.Second {
		t.Fatalf("worst-case retry window = %v; the retry is meant to be bounded to about two minutes", worst)
	}

	policy.jitter = nil
	if got := policy.delay(1000); got != credentialRetryMaxDelay {
		t.Fatalf("delay(1000) = %v, want the %v cap", got, credentialRetryMaxDelay)
	}
	capped := retryPolicy{base: 2 * time.Second, max: 5 * time.Second}
	if capped.delay(1) != 2*time.Second || capped.delay(2) != 4*time.Second || capped.delay(3) != 5*time.Second {
		t.Fatal("delays must double and then stop at the cap")
	}
}

// The synchronous read and every retry share one limit and one client shape.
func TestProductionWiringKeepsTheGuarantees(t *testing.T) {
	deps := productionInitDeps()
	if deps.getenv == nil || deps.readToken == nil || deps.newProvider == nil || deps.logf == nil || deps.handle == nil {
		t.Fatalf("production deps incomplete: %+v", deps)
	}
	if deps.retry.attempts != credentialRetryAttempts || deps.retry.base != credentialRetryBaseDelay || deps.retry.max != credentialRetryMaxDelay {
		t.Fatalf("production retry policy = %+v", deps.retry)
	}
	if credentialReadTimeout != 2*time.Second {
		t.Fatalf("per-attempt limit changed to %v", credentialReadTimeout)
	}
	client := newOpenObserveTokenHTTPClient()
	if client.Timeout != credentialReadTimeout {
		t.Fatalf("client timeout = %v, want %v", client.Timeout, credentialReadTimeout)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("the vault read must stay on the direct transport")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTransientCredentialErrorClassification(t *testing.T) {
	status := func(code int) error {
		return &secrets.StatusError{Name: "openobserve_otlp_ingestion_token", StatusCode: code}
	}
	get := func(err error) error {
		return fmt.Errorf("secrets: request %q: %w", "openobserve_otlp_ingestion_token", err)
	}
	vaultURL := "https://fleet-secrets.0exec.com/secrets/openobserve_otlp_ingestion_token"

	transient := map[string]error{
		"500":                          status(500),
		"502":                          status(502),
		"503":                          status(503),
		"504":                          status(504),
		"599":                          status(599),
		"408 request timeout":          status(http.StatusRequestTimeout),
		"429 too many requests":        status(http.StatusTooManyRequests),
		"wrapped 503":                  fmt.Errorf("outer: %w", status(503)),
		"deadline exceeded":            context.DeadlineExceeded,
		"client timeout":               get(&url.Error{Op: "Get", URL: vaultURL, Err: timeoutError{}}),
		"connection refused":           get(&url.Error{Op: "Get", URL: vaultURL, Err: errors.New("connect: connection refused")}),
		"dial error":                   &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset by peer")},
		"dns failure":                  &net.DNSError{Err: "no such host", Name: "fleet-secrets.0exec.com", IsNotFound: true},
		"body cut short":               fmt.Errorf("secrets: decode %q: %w", "x", fmt.Errorf("response: decode envelope: %w", io.ErrUnexpectedEOF)),
		"bare timeout":                 timeoutError{},
		"tls failure inside url.Error": get(&url.Error{Op: "Get", URL: vaultURL, Err: errors.New("tls: handshake failure")}),
	}
	for name, err := range transient {
		if !isTransientCredentialError(err) {
			t.Errorf("%s must be transient: %v", name, err)
		}
	}

	permanent := map[string]error{
		"nil":                        nil,
		"400":                        status(400),
		"401":                        status(401),
		"403":                        status(403),
		"404":                        status(404),
		"405":                        status(405),
		"410":                        status(410),
		"422":                        status(422),
		"redirect is never followed": status(302),
		"wrapped 403":                fmt.Errorf("outer: %w", status(403)),
		"cancelled":                  context.Canceled,
		"cancelled request":          get(&url.Error{Op: "Get", URL: vaultURL, Err: context.Canceled}),
		"empty value":                errors.New(`secrets: "openobserve_otlp_ingestion_token" present but value is empty`),
		"empty body":                 fmt.Errorf("secrets: decode %q: %w", "x", io.EOF),
		"error envelope":             errors.New("response: status=error with no error detail"),
		"unset base URL":             errors.New("secrets: base URL unset"),
	}
	for name, err := range permanent {
		if isTransientCredentialError(err) {
			t.Errorf("%s must not be retried: %v", name, err)
		}
	}
}

// The reasons that reach log lines come from a fixed vocabulary: error text,
// which can carry URLs, never does.
func TestCredentialFailureReasonUsesAFixedVocabulary(t *testing.T) {
	dirty := "https://user:hunter2@fleet-secrets.0exec.com/secrets/x?token=abc123"
	cases := []struct {
		err  error
		want string
	}{
		{nil, "empty credential"},
		{&secrets.StatusError{Name: "x", StatusCode: 403}, "HTTP 403"},
		{fmt.Errorf("wrapped: %w", &secrets.StatusError{Name: "x", StatusCode: 503}), "HTTP 503"},
		{context.DeadlineExceeded, "timeout"},
		{&url.Error{Op: "Get", URL: dirty, Err: timeoutError{}}, "timeout"},
		{&url.Error{Op: "Get", URL: dirty, Err: errors.New("connection refused")}, "connection error"},
		{fmt.Errorf("decode: %w", io.ErrUnexpectedEOF), "connection dropped"},
		{errors.New("response: status=error with no error detail " + dirty), "unusable response"},
	}
	for _, tc := range cases {
		got := credentialFailureReason(tc.err)
		if got != tc.want {
			t.Errorf("credentialFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
		for _, leak := range []string{"hunter2", "abc123", "fleet-secrets", "token="} {
			if strings.Contains(got, leak) {
				t.Errorf("reason %q leaked %q", got, leak)
			}
		}
	}
}

// The production read path (readOpenObserveToken: direct transport, no
// redirects, 2 s limit) against each real way a vault read can end, with the
// same classification Init's retry applies.
func TestProductionReadClassifiesRealFailureModes(t *testing.T) {
	serve := func(t *testing.T, handler http.HandlerFunc) string {
		t.Helper()
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server.URL
	}
	read := func(baseURL string) (string, error) {
		return readOpenObserveToken(context.Background(), baseURL, testVaultKey)
	}

	t.Run("healthy", func(t *testing.T) {
		got, err := read(serve(t, func(w http.ResponseWriter, _ *http.Request) { writeVaultToken(w, testVaultToken) }))
		if err != nil || got != testVaultToken {
			t.Fatalf("read = %q, %v", got, err)
		}
	})
	t.Run("server error is transient", func(t *testing.T) {
		_, err := read(serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
		if !isTransientCredentialError(err) || credentialFailureReason(err) != "HTTP 503" {
			t.Fatalf("503: transient=%v reason=%q err=%v", isTransientCredentialError(err), credentialFailureReason(err), err)
		}
	})
	t.Run("forbidden is a configuration error", func(t *testing.T) {
		_, err := read(serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
		if err == nil || isTransientCredentialError(err) || credentialFailureReason(err) != "HTTP 403" {
			t.Fatalf("403: transient=%v reason=%q err=%v", isTransientCredentialError(err), credentialFailureReason(err), err)
		}
	})
	t.Run("redirect is neither followed nor retried", func(t *testing.T) {
		var followed atomic.Bool
		elsewhere := serve(t, func(http.ResponseWriter, *http.Request) { followed.Store(true) })
		_, err := read(serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere+"/steal", http.StatusFound)
		}))
		if err == nil || isTransientCredentialError(err) {
			t.Fatalf("302: transient=%v err=%v", isTransientCredentialError(err), err)
		}
		if followed.Load() {
			t.Fatal("the vault read followed a redirect: the API key would have been forwarded")
		}
	})
	t.Run("connection refused is transient", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		dead := server.URL
		server.Close()
		_, err := read(dead)
		if !isTransientCredentialError(err) || credentialFailureReason(err) != "connection error" {
			t.Fatalf("refused: transient=%v reason=%q err=%v", isTransientCredentialError(err), credentialFailureReason(err), err)
		}
	})
	t.Run("slower than the per-attempt limit is transient", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits out the real 2 s limit")
		}
		base := serve(t, func(_ http.ResponseWriter, r *http.Request) { hang(r) })
		begun := time.Now()
		_, err := read(base)
		took := time.Since(begun)
		if !isTransientCredentialError(err) || credentialFailureReason(err) != "timeout" {
			t.Fatalf("hang: transient=%v reason=%q err=%v", isTransientCredentialError(err), credentialFailureReason(err), err)
		}
		if took < credentialReadTimeout-200*time.Millisecond || took > credentialReadTimeout+2*time.Second {
			t.Fatalf("the attempt took %v, want about the %v limit", took, credentialReadTimeout)
		}
	})
}

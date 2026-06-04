package safehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WithTraceCollector configures auto-emit of call traces to
// go-fleet-call-tracer (ADR-0011). Each completed request POSTs a
// trace record to <url>/traces with the fields the tracer's POST
// /traces handler expects: {trace_id, span_id, from_service,
// to_service, method, path, status, duration_ms, ts}. Async,
// fire-and-forget — must NOT block the request. Failure to emit is
// logged at most once per minute (rate-limited) and silently dropped.
//
// Recommended: read CALL_TRACER_URL from env in main.go and pass
// here. If env unset, skip the option — safehttp falls through to
// current behaviour.
func WithTraceCollector(url string) Option {
	return func(o *options) { o.traceURL = strings.TrimRight(url, "/") }
}

// WithBackoffCoordinator configures consultation with
// go-fleet-backoff-coordinator (ADR-0013) before each retry attempt
// against a host that recently returned 5xx or 429. safehttp POSTs
// <url>/backoff with {host, last_response:{status, retry_after_header,
// ts}} and sleeps up to {wait_ms} (capped) in the response before the
// retry attempt. Coordinator outage = fall through to local backoff
// (current behaviour); never blocks indefinitely.
//
// Recommended: read BACKOFF_COORDINATOR_URL from env.
func WithBackoffCoordinator(url string) Option {
	return func(o *options) { o.backoffURL = strings.TrimRight(url, "/") }
}

// WithDegradedSink wires a caller-passed *[]string slice that gets
// "<callee-host>-down" appended on 5xx or network-timeout responses.
// The caller is expected to surface this in its own response (e.g.
// degraded[] in the JSON envelope) so consumers know which sibling
// silently fell back to local logic.
//
// Append is concurrency-safe (mu-protected internally). Caller owns
// the slice lifecycle and is responsible for resetting it per
// request.
//
// Recommended call site:
//
//	var degraded []string
//	c := safehttp.NewClient(safehttp.WithDegradedSink(&degraded), ...)
//	... handle the request, surface degraded in the response ...
func WithDegradedSink(sink *[]string) Option {
	return func(o *options) { o.degradedSink = sink }
}

// extrasTransport wraps the underlying RoundTripper with the three
// opt-in fleet hooks: backoff coordination (pre-call), trace
// emission (post-call, async) and degraded-sink append (post-call,
// sync). It is only inserted in the chain when at least one of the
// three knobs is configured — clients with no new options get an
// identical transport stack to v0.15.0.
type extrasTransport struct {
	inner http.RoundTripper

	traceURL   string
	backoffURL string

	degradedSink *[]string
	degradedMu   sync.Mutex

	// observer (optional) receives one EgressEvent per round-trip
	// attempt. Used by promx to record fleet-canonical Prometheus
	// metrics without coupling safehttp to client_golang.
	observer EgressObserver

	// fetchDelegate (optional, per-client) routes eligible outbound GETs
	// (no body, no Range header) through an alternate fetcher — the fleet
	// fetch cache — so many services fetching the same URL collapse to one
	// origin fetch. On a delegate error/nil result we fall through to the
	// normal direct path: a cache outage must never break a request.
	// Delegated GETs do NOT emit the safehttp egress observer (the fetch
	// happened in the cache, which emits its own fleet_fetch_cache_*
	// metrics). An explicit per-client WithFetchDelegate wins over the
	// process-wide default and applies even to withoutProxy clients.
	fetchDelegate FetchDelegate
	// useDefaultFetchCache, when true (set unless WithoutProxy /
	// WithoutFetchCache), consults the process-wide DefaultFetchDelegate
	// AT CALL TIME — mirroring the DefaultObserver resolution below — so a
	// delegate installed AFTER NewClient (the common server.New flow, vs.
	// package-level var clients built at import) still routes through it.
	useDefaultFetchCache bool
	// proxyFn is the same function passed to http.Transport.Proxy so
	// the observer can label each request with via_proxy / proxy_host.
	// nil means WithoutProxy was set (always direct).
	proxyFn func(*http.Request) (*url.URL, error)

	caller string // service slug derived from User-Agent

	// hostState tracks the last bad response per host so the
	// coordinator only gets consulted for follow-up calls (its
	// purpose is to coordinate retries, not gate every request).
	hostMu    sync.Mutex
	hostState map[string]hostFailure

	// trace-emit failure log rate-limiter (unix seconds)
	lastTraceErrLog atomic.Int64

	// fetch-cache debug log rate-limiter (unix seconds). Only used when
	// SAFEHTTP_FETCHCACHE_DEBUG is set — see fetchCacheDebug.
	lastFetchCacheDbgLog atomic.Int64
}

// fetchCacheDebug, when true (env SAFEHTTP_FETCHCACHE_DEBUG set at startup),
// makes the transport log its per-GET fetch-cache routing decision —
// whether the delegate engaged, routed through the cache, or fell through
// to direct egress. A diagnostic aid for confirming a service actually
// routes through the fleet fetch cache; off by default, rate-limited.
var fetchCacheDebug = os.Getenv("SAFEHTTP_FETCHCACHE_DEBUG") != ""

// logFetchCacheDebug emits a fetch-cache routing decision line, rate-limited
// to ~once / 2s so a busy client can't flood the log.
func (t *extrasTransport) logFetchCacheDebug(format string, args ...any) {
	now := time.Now().Unix()
	prev := t.lastFetchCacheDbgLog.Load()
	if now-prev < 2 {
		return
	}
	if !t.lastFetchCacheDbgLog.CompareAndSwap(prev, now) {
		return
	}
	log.Printf("safehttp fetchcache: "+format, args...)
}

type hostFailure struct {
	status            int
	retryAfterSeconds int
	ts                time.Time
}

const (
	coordinatorConnectTimeout = 500 * time.Millisecond
	coordinatorReadTimeout    = 1 * time.Second
	// maxBackoffSleep caps how long we wait on the coordinator's
	// advice — fail-open contract: a runaway coordinator must
	// never wedge a caller indefinitely.
	maxBackoffSleep = 5 * time.Second
	// hostFailureTTL bounds how long a recent failure stays
	// "interesting" for coordinator consultation.
	hostFailureTTL = 2 * time.Minute
)

func (t *extrasTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	// Fetch-cache routing: a plain GET (no request body, no Range
	// header) is eligible to be served by the fetch-cache delegate
	// instead of hitting origin directly. This is what lets many
	// services fetching the same URL collapse to one origin fetch
	// (the cache does server-side singleflight + caching). On any
	// delegate error or nil result we FALL THROUGH to the normal
	// direct path below — a cache outage must never break a request.
	// Per-client delegate takes precedence; otherwise fall back to the
	// process-wide DefaultFetchDelegate resolved AT CALL TIME (unless the
	// client opted out via WithoutProxy / WithoutFetchCache), so package-
	// level var clients built before server.New still route through it.
	delegate := t.fetchDelegate
	if delegate == nil && t.useDefaultFetchCache {
		delegate = DefaultFetchDelegate()
	}
	eligibleGet := req.Method == http.MethodGet && req.Body == nil && req.Header.Get("Range") == ""
	if fetchCacheDebug && eligibleGet {
		t.logFetchCacheDebug("decision host=%s perClientDelegate=%v useDefaultFetchCache=%v defaultDelegateInstalled=%v willRoute=%v",
			host, t.fetchDelegate != nil, t.useDefaultFetchCache, DefaultFetchDelegate() != nil, delegate != nil)
	}
	if delegate != nil && eligibleGet {
		res, err := delegate.FetchGet(req.Context(), req.URL.String(), req.Header)
		if err == nil && res != nil {
			if fetchCacheDebug {
				t.logFetchCacheDebug("routed via cache host=%s status=%d bytes=%d", host, res.Status, len(res.Body))
			}
			return &http.Response{
				StatusCode:    res.Status,
				Status:        http.StatusText(res.Status),
				Header:        cloneOrEmptyHeader(res.Header),
				Body:          io.NopCloser(bytes.NewReader(res.Body)),
				ContentLength: int64(len(res.Body)),
				Request:       req,
				Proto:         "HTTP/1.1",
				ProtoMajor:    1,
				ProtoMinor:    1,
			}, nil
		}
		// delegate error / nil → fall through to direct egress.
		if fetchCacheDebug {
			t.logFetchCacheDebug("delegate fell through to direct host=%s err=%v", host, err)
		}
	}

	// Pre-call: if we've recently observed a 5xx/429 for this host,
	// consult the backoff coordinator. Fail-open on any error.
	if t.backoffURL != "" {
		if fail, ok := t.recentFailure(host); ok {
			start := time.Now()
			waited := t.consultBackoff(req.Context(), host, fail)
			if obs := DefaultBackoffObserver(); obs != nil {
				outcome := "consulted_no_wait"
				switch {
				case waited < 0:
					outcome = "unreachable"
				case waited > 0:
					outcome = "consulted_waited"
				}
				obs.ObserveBackoff(BackoffEvent{
					Host:           host,
					PriorStatus:    fail.status,
					Outcome:        outcome,
					ConsultLatency: time.Since(start),
					Waited:         waited,
				})
			}
		}
	}

	start := time.Now()
	resp, err := t.inner.RoundTrip(req)
	dur := time.Since(start)

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}

	// Network timeouts and transport errors count as "host down"
	// for the degraded-sink + host-state tracking.
	isNetErr := err != nil
	is5xx := status >= 500 && status <= 599
	is429 := status == 429

	if is5xx || is429 || isNetErr {
		retryAfter := 0
		if resp != nil {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		t.recordFailure(host, status, retryAfter)
		if (is5xx || isNetErr) && t.degradedSink != nil {
			t.appendDegraded(host)
		}
	} else if status > 0 {
		t.clearFailure(host)
	}

	// Observer emit — inline, on the hot path. Implementations are
	// contracted to be cheap and non-blocking (counter/histogram
	// observations only). We deliberately call this BEFORE the async
	// trace emit so failures in trace emission can't reorder the
	// observation.
	//
	// Per-client observer takes precedence; otherwise fall back to the
	// process-wide DefaultObserver resolved AT CALL TIME so that
	// observers installed AFTER NewClient (the common server.New →
	// safehttp.SetDefaultObserver flow, vs. package-level var clients
	// constructed at init) are still seen.
	obs := t.observer
	if obs == nil {
		obs = DefaultObserver()
	}
	if obs != nil {
		viaProxy, proxyHost := t.resolveProxy(req)
		obs.ObserveEgress(EgressEvent{
			Method:    req.Method,
			Host:      host,
			Scheme:    req.URL.Scheme,
			Path:      req.URL.Path,
			Status:    status,
			Duration:  dur,
			Bytes:     responseBytes(resp),
			ViaProxy:  viaProxy,
			ProxyHost: proxyHost,
			Outcome:   classifyOutcome(status, err),
			Err:       err,
		})
	}

	// Async trace emit — never blocks the response. Snapshot the
	// fields needed so the goroutine doesn't race with the caller
	// consuming the response.
	if t.traceURL != "" {
		method := req.Method
		path := req.URL.Path
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		go t.emitTrace(traceFields{
			Caller:     t.caller,
			Host:       host,
			Method:     method,
			Path:       path,
			Status:     status,
			DurationMs: dur.Milliseconds(),
			TS:         start.UTC().Format(time.RFC3339Nano),
			Err:        errStr,
		})
	}

	return resp, err
}

func (t *extrasTransport) appendDegraded(host string) {
	if host == "" {
		return
	}
	t.degradedMu.Lock()
	defer t.degradedMu.Unlock()
	*t.degradedSink = append(*t.degradedSink, host+"-down")
}

func (t *extrasTransport) recordFailure(host string, status, retryAfter int) {
	if host == "" {
		return
	}
	t.hostMu.Lock()
	defer t.hostMu.Unlock()
	if t.hostState == nil {
		t.hostState = make(map[string]hostFailure)
	}
	t.hostState[host] = hostFailure{
		status:            status,
		retryAfterSeconds: retryAfter,
		ts:                time.Now(),
	}
}

func (t *extrasTransport) clearFailure(host string) {
	t.hostMu.Lock()
	defer t.hostMu.Unlock()
	if t.hostState != nil {
		delete(t.hostState, host)
	}
}

func (t *extrasTransport) recentFailure(host string) (hostFailure, bool) {
	t.hostMu.Lock()
	defer t.hostMu.Unlock()
	if t.hostState == nil {
		return hostFailure{}, false
	}
	f, ok := t.hostState[host]
	if !ok {
		return hostFailure{}, false
	}
	if time.Since(f.ts) > hostFailureTTL {
		delete(t.hostState, host)
		return hostFailure{}, false
	}
	return f, true
}

// consultBackoff POSTs the coordinator and sleeps up to wait_ms
// (capped by maxBackoffSleep). Bounded by coordinatorConnectTimeout
// + coordinatorReadTimeout overall so a hung coordinator never
// escalates to the caller.
func (t *extrasTransport) consultBackoff(parentCtx context.Context, host string, fail hostFailure) (waited time.Duration) {
	body := map[string]any{
		"host": host,
		"last_response": map[string]any{
			"status":             fail.status,
			"retry_after_header": fail.retryAfterSeconds,
			"ts":                 fail.ts.UTC().Format(time.RFC3339Nano),
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return -1
	}

	// Hard cap: connect + read budget. Never block beyond this.
	overall := coordinatorConnectTimeout + coordinatorReadTimeout
	ctx, cancel := context.WithTimeout(parentCtx, overall)
	defer cancel()

	cli := &http.Client{
		Timeout: overall,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: coordinatorConnectTimeout,
			}).DialContext,
			ResponseHeaderTimeout: coordinatorReadTimeout,
			DisableKeepAlives:     true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.backoffURL+"/backoff", bytes.NewReader(buf))
	if err != nil {
		return -1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return -1 // fail-open: silent fall-through
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var dec struct {
		WaitMS int64 `json:"wait_ms"`
	}
	lim := io.LimitReader(resp.Body, 1<<14)
	if err := json.NewDecoder(lim).Decode(&dec); err != nil {
		return 0
	}
	if dec.WaitMS <= 0 {
		return 0
	}
	wait := time.Duration(dec.WaitMS) * time.Millisecond
	if wait > maxBackoffSleep {
		wait = maxBackoffSleep
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return wait
	case <-parentCtx.Done():
		return 0
	}
}

type traceFields struct {
	Caller     string
	Host       string
	Method     string
	Path       string
	Status     int
	DurationMs int64
	TS         string
	Err        string
}

func (t *extrasTransport) emitTrace(s traceFields) {
	defer func() {
		// Belt-and-braces: a panicking trace emit must never
		// crash the calling service.
		if r := recover(); r != nil {
			t.maybeLogTraceErr("panic: %v", r)
		}
	}()

	// Tracer expects {"spans":[{...}]} with from_service/to_service
	// fields and an opaque trace_id/span_id. We mint a synthetic
	// pair here — full distributed-tracing IDs would require
	// context-propagation plumbing the caller doesn't have today.
	span := map[string]any{
		"trace_id":     randomID(),
		"span_id":      randomID(),
		"from_service": s.Caller,
		"to_service":   s.Host,
		"method":       s.Method,
		"path":         s.Path,
		"status":       s.Status,
		"duration_ms":  s.DurationMs,
		"ts":           s.TS,
	}
	if s.Err != "" {
		span["error"] = s.Err
	}
	body, err := json.Marshal(map[string]any{"spans": []any{span}})
	if err != nil {
		t.maybeLogTraceErr("marshal: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), coordinatorConnectTimeout+coordinatorReadTimeout)
	defer cancel()

	cli := &http.Client{
		Timeout: coordinatorConnectTimeout + coordinatorReadTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: coordinatorConnectTimeout,
			}).DialContext,
			ResponseHeaderTimeout: coordinatorReadTimeout,
			DisableKeepAlives:     true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.traceURL+"/traces", bytes.NewReader(body))
	if err != nil {
		t.maybeLogTraceErr("newrequest: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.maybeLogTraceErr("post: %v", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode >= 400 {
		t.maybeLogTraceErr("collector returned %d", resp.StatusCode)
	}
}

// maybeLogTraceErr rate-limits trace-emit failure logs to at most
// once per minute per client so a down collector does not flood
// stderr.
func (t *extrasTransport) maybeLogTraceErr(format string, args ...any) {
	now := time.Now().Unix()
	prev := t.lastTraceErrLog.Load()
	if now-prev < 60 {
		return
	}
	if !t.lastTraceErrLog.CompareAndSwap(prev, now) {
		return
	}
	log.Printf("safehttp: trace emit failed: "+format, args...)
}

// callerFromUA pulls the leading "<service-id>" slug out of a
// ua.Build-shaped User-Agent string (which is
// "<service-id>/<version> (...)"). Returns "" if the input is empty
// or doesn't match the expected shape — callers tolerate that.
func callerFromUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	// Take the token up to the first space, then strip "/version".
	first := ua
	if i := strings.IndexByte(ua, ' '); i > 0 {
		first = ua[:i]
	}
	if i := strings.IndexByte(first, '/'); i > 0 {
		return first[:i]
	}
	return first
}

// parseRetryAfter accepts the integer-seconds form of the
// Retry-After header. HTTP-date form is ignored (returns 0) — the
// coordinator can still apply its own policy.
func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0
		}
	}
	return n
}

// randomID returns a short hex id. Used for synthetic trace and
// span ids — the tracer treats them as opaque strings.
func randomID() string {
	// Avoid crypto/rand to keep this off the hot path; trace IDs
	// only need to be unique-enough for debugging, not unguessable.
	now := time.Now().UnixNano()
	return hex16(uint64(now)) + hex16(idCounter.Add(1))
}

var idCounter atomic.Uint64

// resolveProxy mirrors what http.Transport will do internally: invoke the
// configured Proxy func to decide if this request goes through a proxy. We
// invoke the same function rather than introspect Transport state because
// it's the only source of truth for env-var resolution (NO_PROXY,
// HTTPS_PROXY vs HTTP_PROXY, scheme/host matching). Errors fall back to
// "direct"; that matches Go's own behaviour.
func (t *extrasTransport) resolveProxy(req *http.Request) (viaProxy bool, proxyHost string) {
	if t.proxyFn == nil {
		return false, ""
	}
	u, err := t.proxyFn(req)
	if err != nil || u == nil {
		return false, ""
	}
	return true, u.Host
}

// cloneOrEmptyHeader returns a clone of h, or a fresh empty header when
// h is nil — so the synthesized *http.Response always carries a usable
// (and caller-mutable, non-aliased) Header map.
func cloneOrEmptyHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	return h.Clone()
}

// responseBytes returns Content-Length if set and parseable; 0 otherwise.
// We deliberately don't drain the body — that would change request
// semantics for the caller. Histograms record what's known; unknown is 0.
func responseBytes(resp *http.Response) int64 {
	if resp == nil {
		return 0
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}

// classifyOutcome buckets a (status, err) pair into a small label-safe set.
// Order matters: error classification first (network/SSRF/etc), then HTTP
// status falls through. Returns OutcomeSuccess for 2xx, OutcomeRedirect for
// 3xx (which only reaches here after CheckRedirect lets it through).
func classifyOutcome(status int, err error) EgressOutcome {
	if err != nil {
		switch {
		case errors.Is(err, ErrBlocked):
			return OutcomeBlocked
		case isTimeoutErr(err):
			return OutcomeTimeout
		case isTLSErr(err):
			return OutcomeTLSFail
		case isDNSErr(err):
			return OutcomeDNSFail
		default:
			return OutcomeNetError
		}
	}
	switch {
	case status >= 200 && status < 300:
		return OutcomeSuccess
	case status >= 300 && status < 400:
		return OutcomeRedirect
	case status >= 400 && status < 500:
		return OutcomeClientError
	case status >= 500 && status < 600:
		return OutcomeServerError
	}
	return OutcomeNetError
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

func isTLSErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "tls:") || strings.Contains(s, "x509:")
}

func isDNSErr(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func hex16(v uint64) string {
	const hexd = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = hexd[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

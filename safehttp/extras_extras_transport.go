package safehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// coordinatorClient is the shared HTTP client for the fleet coordinator and
// tracer POSTs (backoff consult + trace emission). It is built once and
// reused so those internal calls pool keep-alive connections instead of
// doing a fresh TCP(+TLS) connect per call — the previous code allocated a
// new http.Client + http.Transport with DisableKeepAlives on every single
// invocation, so a service degraded against an upstream opened a brand-new
// connection to the coordinator on every request. Per-call deadlines are
// enforced via the request context (see consultBackoff/emitTrace), so this
// client carries no fixed Timeout. SSRF/proxy guards intentionally do NOT
// apply: these are explicit calls to known intra-mesh coordinator URLs, the
// same posture the fleet uses for all sibling-service calls.
var coordinatorClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: coordinatorConnectTimeout}).DialContext,
		ResponseHeaderTimeout: coordinatorReadTimeout,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
	},
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
	// Per-request opt-out: WithoutFetchCacheContext(ctx) bypasses the
	// process-wide default delegate (e.g. selftest probes validating the
	// real origin path). An explicit per-client WithFetchDelegate was wired
	// on purpose, so it still wins over the context flag.
	if t.fetchDelegate == nil && fetchCacheDisabledByContext(req.Context()) {
		delegate = nil
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

	cli := coordinatorClient
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

	cli := coordinatorClient
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

package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// fetchCacheDebug, when true (env SAFEHTTP_FETCHCACHE_DEBUG set at startup),
// makes the transport log its per-GET fetch-cache routing decision —
// whether the delegate engaged, routed through the cache, or fell through
// to direct egress. A diagnostic aid for confirming a service actually
// routes through the fleet fetch cache; off by default, rate-limited.
var fetchCacheDebug = os.Getenv("SAFEHTTP_FETCHCACHE_DEBUG") != ""

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
// This is now used only as the countingBody fallback (see below) for a
// caller that closes the body without reading it — NOT as the primary byte
// signal. It used to be the only signal, which silently reported 0 for any
// chunked/gzip'd response with no Content-Length header (most dynamic HTML)
// — the actual bytes were always available, just never read.
func responseBytes(resp *http.Response) int64 {
	if resp == nil {
		return 0
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}

// countingBody wraps a response body so the EgressObserver sees actual bytes
// read rather than a Content-Length guess, WITHOUT changing read semantics
// for the caller (still a normal lazy io.ReadCloser; nothing is pre-drained).
// The final count is only definitive at Close() — that's the one point in
// the http.Response contract every well-behaved caller is guaranteed to
// reach — so onClose (which fires the deferred EgressEvent) runs there,
// exactly once.
//
// Caveat, by design: this measures bytes the CALLER actually consumed from
// resp.Body, not bytes that crossed the wire. For a caller that reads only
// part of the body then closes early (a size-capped reader, e.g.
// io.LimitReader(resp.Body, N) followed by Close() without reading further),
// this undercounts relative to true network egress — TCP may have buffered
// more than userspace ever called Read() for. This is an inherent limit of
// application-layer measurement; cross-check against host/cgroup-level
// network byte counters (e.g. cAdvisor) for ground truth, same as this
// package's own doc guidance elsewhere about trusting one signal alone.
//
// fallback (Content-Length) is used ONLY when zero bytes were ever read —
// that's "the caller chose not to consume the body" (a HEAD-style
// status/header-only check), a real and already-relied-upon case, distinct
// from "zero bytes transferred". See safehttp/observer_test.go's
// TestObserverFiresOnSuccess, which asserts exactly this fallback.
type countingBody struct {
	io.ReadCloser
	n         int64
	fallback  int64
	closeOnce sync.Once
	onClose   func(n int64)
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		atomic.AddInt64(&c.n, int64(n))
	}
	return n, err
}

func (c *countingBody) Close() error {
	err := c.ReadCloser.Close()
	c.closeOnce.Do(func() {
		if c.onClose == nil {
			return
		}
		n := atomic.LoadInt64(&c.n)
		if n == 0 && c.fallback > 0 {
			n = c.fallback
		}
		c.onClose(n)
	})
	return err
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

// Package backoffcoord is the fleet client for
// go-fleet-backoff-coordinator (ADR-0013). Where ratecoord coordinates
// per-host token budgets, backoffcoord coordinates per-host *post-
// failure* sleep so a shared upstream that started 5xx-ing doesn't get
// hammered by every fleet caller's local retry.
//
// safehttp's WithBackoffCoordinator option already speaks this
// protocol, but it's hard-wired inside the transport — services that
// want to consult the coordinator from non-HTTP code paths (e.g. a
// queue worker re-trying a job, a DNS resolver wrapper) have no
// canonical client to call. This package extracts the wire protocol
// into a reusable shape with an Observer for fleet metrics.
//
// Design centre: fail-open, cheap, observable. Consult never blocks
// beyond the connect+read budget. On any error the response is "wait
// 0" so callers can proceed; the Observer fires "unreachable" so
// fleet metrics show the coordinator is degraded.
package backoffcoord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultURL              = "https://backoff-coordinator.0exec.com"
	defaultConnectTimeout   = 500 * time.Millisecond
	defaultReadTimeout      = 1 * time.Second
	defaultMaxWait          = 5 * time.Second
)

// Client talks to a backoff-coordinator instance. Construct one per
// process and reuse — no per-call allocation beyond the request body.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// MaxWait caps how long any single Consult call will block on the
	// coordinator's advice. Defaults to 5s; values <= 0 use the default.
	MaxWait time.Duration

	observer Observer
}

// New constructs a client from env vars. Safe at package init.
//
//	BACKOFF_COORDINATOR_URL  default https://backoff-coordinator.0exec.com
//	BACKOFF_COORDINATOR_MAX_WAIT_MS  optional; default 5000
func New() *Client {
	base := os.Getenv("BACKOFF_COORDINATOR_URL")
	if base == "" {
		base = DefaultURL
	}
	maxWait := defaultMaxWait
	return &Client{
		BaseURL: strings.TrimRight(base, "/"),
		HTTP: &http.Client{
			Timeout: defaultConnectTimeout + defaultReadTimeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: defaultConnectTimeout}).DialContext,
				ResponseHeaderTimeout: defaultReadTimeout,
				DisableKeepAlives:     true,
			},
		},
		MaxWait: maxWait,
	}
}

// SetObserver attaches an Observer to the client. Idempotent.
func (c *Client) SetObserver(o Observer) *Client {
	c.observer = o
	return c
}

// Failure describes the most recent bad response from the host the
// caller is about to retry. status==0 means "network error / timeout";
// retryAfterSec is the integer-seconds form of the upstream's Retry-
// After header (0 if absent).
type Failure struct {
	Status        int
	RetryAfterSec int
	At            time.Time
}

// Result is what Consult returns. Wait is what the caller should
// sleep before retrying. FellOpen is true when the coordinator was
// unreachable and we returned wait=0 to keep the caller moving.
type Result struct {
	Wait     time.Duration
	FellOpen bool
	Reason   string
}

// ErrNoHost is returned by Consult when host is empty.
var ErrNoHost = errors.New("backoffcoord: host required")

// Consult asks the coordinator how long to sleep before retrying a
// failed call to host. Never blocks beyond MaxWait + the connect+read
// budget.
//
// Returns nil error on success (advice obtained or coordinator
// unreachable — both are non-fatal). Returns ctx.Err() if the parent
// context cancels first.
func (c *Client) Consult(ctx context.Context, host string, fail Failure) (*Result, error) {
	if host == "" {
		return nil, ErrNoHost
	}
	start := time.Now()
	res := &Result{}
	defer func() {
		c.emit(Event{
			Host:           host,
			Outcome:        outcomeFromResult(res),
			ConsultLatency: time.Since(start),
			Waited:         res.Wait,
		})
	}()

	body, err := json.Marshal(map[string]any{
		"host": host,
		"last_response": map[string]any{
			"status":             fail.Status,
			"retry_after_header": fail.RetryAfterSec,
			"ts":                 fail.At.UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		res.FellOpen = true
		res.Reason = "marshal error"
		return res, nil
	}

	cctx, cancel := context.WithTimeout(ctx, defaultConnectTimeout+defaultReadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.BaseURL+"/backoff", bytes.NewReader(body))
	if err != nil {
		res.FellOpen = true
		res.Reason = "request build error"
		return res, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		res.FellOpen = true
		res.Reason = "unreachable: " + err.Error()
		return res, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		res.Reason = "non-200"
		return res, nil
	}

	var dec struct {
		WaitMS int64 `json:"wait_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&dec); err != nil {
		res.Reason = "decode error"
		return res, nil
	}
	if dec.WaitMS <= 0 {
		return res, nil
	}
	wait := time.Duration(dec.WaitMS) * time.Millisecond
	cap := c.MaxWait
	if cap <= 0 {
		cap = defaultMaxWait
	}
	if wait > cap {
		wait = cap
	}
	res.Wait = wait
	return res, nil
}

// Sleep is a convenience wrapper that calls Consult and then sleeps
// the recommended duration (bounded by ctx). Useful for the common
// "if recently-failed, then back off" pattern outside HTTP transports.
func (c *Client) Sleep(ctx context.Context, host string, fail Failure) (*Result, error) {
	res, err := c.Consult(ctx, host, fail)
	if err != nil || res == nil || res.Wait <= 0 {
		return res, err
	}
	timer := time.NewTimer(res.Wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	return res, nil
}

// Observer receives one event per Consult call. Implementations MUST
// NOT block.
type Observer interface {
	ObserveBackoffCoord(Event)
}

// Event is the per-Consult payload handed to an Observer. Outcome is
// one of: "no_wait", "waited", "unreachable".
type Event struct {
	Host           string
	Outcome        string
	ConsultLatency time.Duration
	Waited         time.Duration
}

func outcomeFromResult(r *Result) string {
	switch {
	case r.FellOpen:
		return "unreachable"
	case r.Wait > 0:
		return "waited"
	default:
		return "no_wait"
	}
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (c *Client) emit(ev Event) {
	if c.observer != nil {
		c.observer.ObserveBackoffCoord(ev)
		return
	}
	if obs := DefaultObserver(); obs != nil {
		obs.ObserveBackoffCoord(ev)
	}
}

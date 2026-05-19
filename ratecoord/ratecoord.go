// Package ratecoord is the fleet client for go-pentest-rate-coordinator,
// the per-host token-bucket service. Any service that fans out HTTP
// requests against arbitrary upstream hosts (broken-links, subdomain-
// finder, iframe-analyzer's recursive walk, …) should route through
// this client to share a global budget instead of each service having
// its own private rate limit.
//
// Graceful degradation is the design centre: the coordinator is a
// single-instance service and may be unreachable. Wait() never blocks
// forever and never fails closed by default — on coordinator outage
// the call falls back to a per-process token bucket so the calling
// service keeps making progress, and the FellBack flag on the response
// surfaces the degradation to /health and metrics.
//
// Environment:
//
//	RATECOORD_URL          default https://rate-coordinator.0exec.com
//	RATECOORD_API_KEY      or FLEET_API_KEY (required for real coordinator;
//	                        unused on the in-process fallback path)
//	RATECOORD_DEFAULT_RPS  per-host fallback bucket rate (default 4)
//	RATECOORD_DEFAULT_BURST per-host fallback bucket burst (default 8)
//
// The in-process fallback intentionally uses a different (and stricter)
// default than the coordinator's network policy: when coordination
// breaks down we want each instance to slow down on its own to avoid
// thundering-herd against the upstream.
package ratecoord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultURL   = "https://rate-coordinator.0exec.com"
	DefaultTimeoutMS = 5000
)

// Client talks to a rate-coordinator instance and, on failure, falls
// back to a per-process per-host limiter.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	// Fallback state. Lazily initialised per-host on first failure.
	fbMu sync.Mutex
	fb   map[string]*rate.Limiter

	fbRPS   float64
	fbBurst int

	observer Observer
}

// emit fires the observer if one is attached. Internal helper so the
// hot path stays nil-safe without inline ceremony.
func (c *Client) emit(ev Event) {
	if c.observer != nil {
		c.observer.ObserveRate(ev)
	}
}

// New constructs a client from environment. Safe to call at package init.
func New() *Client {
	base := os.Getenv("RATECOORD_URL")
	if base == "" {
		base = DefaultURL
	}
	apiKey := os.Getenv("RATECOORD_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("FLEET_API_KEY")
	}
	rps, _ := strconv.ParseFloat(os.Getenv("RATECOORD_DEFAULT_RPS"), 64)
	if rps <= 0 {
		rps = 4
	}
	burst, _ := strconv.Atoi(os.Getenv("RATECOORD_DEFAULT_BURST"))
	if burst <= 0 {
		burst = 8
	}
	return &Client{
		BaseURL: base,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		fb:      map[string]*rate.Limiter{},
		fbRPS:   rps,
		fbBurst: burst,
	}
}

// WaitResult conveys what happened during Wait.
type WaitResult struct {
	WaitedMs int64 `json:"waited_ms"`
	// FellBack is true when the coordinator was unreachable and we used
	// the in-process bucket. Surface this via depcheck / metrics so an
	// operator can spot a coordinator outage.
	FellBack bool   `json:"fell_back,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Wait blocks up to maxWait for a token for host. If maxWait <= 0,
// defaults to 5s.
//
// Returns nil error on success (token acquired, either remotely or via
// fallback). Returns context error if ctx cancels first.
func (c *Client) Wait(ctx context.Context, host string, weight int, maxWait time.Duration) (*WaitResult, error) {
	if host == "" {
		return nil, errors.New("ratecoord: host required")
	}
	if weight <= 0 {
		weight = 1
	}
	if maxWait <= 0 {
		maxWait = 5 * time.Second
	}

	// Try the coordinator first.
	res, err := c.waitRemote(ctx, host, weight, maxWait)
	if err == nil {
		c.emit(Event{Host: host, Weight: weight, Waited: time.Duration(res.WaitedMs) * time.Millisecond, Allowed: true, FellBack: false, Reason: res.Reason})
		return res, nil
	}

	// Fall back to a per-process token bucket. We honour the same
	// maxWait budget the caller asked for so callers stay bounded.
	waitCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	start := time.Now()
	lim := c.fallbackLimiter(host)
	if err := lim.WaitN(waitCtx, weight); err != nil {
		out := &WaitResult{
			WaitedMs: time.Since(start).Milliseconds(),
			FellBack: true,
			Reason:   "coordinator unreachable: " + err.Error(),
		}
		c.emit(Event{Host: host, Weight: weight, Waited: time.Duration(out.WaitedMs) * time.Millisecond, Allowed: false, FellBack: true, Reason: out.Reason})
		return out, err
	}
	out := &WaitResult{
		WaitedMs: time.Since(start).Milliseconds(),
		FellBack: true,
		Reason:   "coordinator unreachable: " + err.Error(),
	}
	c.emit(Event{Host: host, Weight: weight, Waited: time.Duration(out.WaitedMs) * time.Millisecond, Allowed: true, FellBack: true, Reason: out.Reason})
	return out, nil
}

func (c *Client) fallbackLimiter(host string) *rate.Limiter {
	c.fbMu.Lock()
	defer c.fbMu.Unlock()
	if l, ok := c.fb[host]; ok {
		return l
	}
	l := rate.NewLimiter(rate.Limit(c.fbRPS), c.fbBurst)
	c.fb[host] = l
	return l
}

type waitRequest struct {
	Host      string `json:"host"`
	Weight    int    `json:"weight"`
	TimeoutMs int    `json:"timeout_ms"`
}

type waitResponse struct {
	WaitedMs int64  `json:"waited_ms"`
	Error    string `json:"error,omitempty"`
}

func (c *Client) waitRemote(ctx context.Context, host string, weight int, maxWait time.Duration) (*WaitResult, error) {
	if c.APIKey == "" {
		return nil, errors.New("ratecoord: API key not set")
	}
	body, _ := json.Marshal(waitRequest{
		Host:      host,
		Weight:    weight,
		TimeoutMs: int(maxWait.Milliseconds()),
	})
	reqURL := strings.TrimRight(c.BaseURL, "/") + "/wait?api_key=" + c.APIKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ratecoord: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ratecoord: do: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ratecoord: status %d: %s", resp.StatusCode, string(raw))
	}
	var out waitResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ratecoord: decode: %w", err)
	}
	return &WaitResult{WaitedMs: out.WaitedMs}, nil
}

// Probe is a depcheck-friendly health check for the coordinator. Returns
// nil if the coordinator's /health endpoint returns 200 within 2 seconds.
func (c *Client) Probe(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reqURL := strings.TrimRight(c.BaseURL, "/") + "/health"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecoord probe: status %d", resp.StatusCode)
	}
	return nil
}

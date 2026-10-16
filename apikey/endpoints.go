package apikey

// endpoints.go — several keystore nodes behind one Client.
//
// The keystore is moving from one instance on one host to a node on every
// server (keystore mesh). A Client can therefore know an ordered list of
// endpoints: the node on its own host first, then the others. Verify asks the
// first healthy endpoint and, if it has not answered within HedgeDelay, also
// asks the next one; the first definitive answer wins. A transport failure or a
// 5xx moves on to the next endpoint at once. A 401 is definitive and is never
// retried elsewhere: every node holds the same keys (a node that has not yet
// seen a freshly issued key asks its peers itself before answering 401).
//
// With a single endpoint nothing changes: one request, the same errors as
// before.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultHedgeDelay is how long Verify waits for one endpoint before it
	// also asks the next one.
	DefaultHedgeDelay = 300 * time.Millisecond

	// HedgeHeader is set to "1" on a verify sent while an earlier attempt for
	// the same check is still in flight. The keystore (go_apikey_service >=
	// 0.7.5) answers such a request for a use-limited key with 409, which is
	// not definitive here, instead of spending a use the first attempt may
	// spend too. A failover after an attempt failed is sent without it.
	HedgeHeader = "X-Keystore-Hedge"

	breakerThreshold = 3                // consecutive transport failures that open an endpoint's breaker
	breakerCooldown  = 30 * time.Second // how long an open endpoint is tried only after the healthy ones
)

// VerifyEvent describes one Verify call for an optional observer.
type VerifyEvent struct {
	Endpoint string        // the endpoint whose answer was used ("" when none answered)
	Result   string        // ok | invalid | unavailable
	Attempts int           // endpoints asked
	Hedged   bool          // a second endpoint was asked because the first was slow or failing
	Duration time.Duration // whole call
}

// VerifyObserver receives one VerifyEvent per Verify call.
type VerifyObserver interface {
	ObserveVerify(VerifyEvent)
}

// endpointList returns the endpoints in configured order (Endpoints, or the
// single BaseURL).
func (c *Client) endpointList() []string {
	if len(c.Endpoints) > 0 {
		return c.Endpoints
	}
	return []string{c.BaseURL}
}

// orderedEndpoints puts endpoints with a closed breaker first, keeping the
// configured order within each group, so a dead node is skipped rather than
// waited on, but is still tried when nothing else is left.
func (c *Client) orderedEndpoints(now time.Time) []string {
	all := c.endpointList()
	if len(all) == 1 {
		return all
	}
	healthy := make([]string, 0, len(all))
	var open []string
	for _, ep := range all {
		if breakerFor(ep).isOpen(now) {
			open = append(open, ep)
		} else {
			healthy = append(healthy, ep)
		}
	}
	return append(healthy, open...)
}

func (c *Client) hedgeDelay() time.Duration {
	if c.HedgeDelay > 0 {
		return c.HedgeDelay
	}
	return DefaultHedgeDelay
}

type verifyOutcome struct {
	endpoint string
	result   *VerifyResult
	err      error
}

// definitive reports whether an answer settles the question: valid, or a 401.
func (o verifyOutcome) definitive() bool {
	return o.err == nil || errors.Is(o.err, ErrInvalidKey)
}

// verifyAcross runs Verify over the ordered endpoints with hedging.
func (c *Client) verifyAcross(ctx context.Context, key string) (*VerifyResult, error) {
	start := time.Now()
	eps := c.orderedEndpoints(start)
	ev := VerifyEvent{}
	defer func() {
		ev.Duration = time.Since(start)
		if c.VerifyObs != nil {
			c.VerifyObs.ObserveVerify(ev)
		}
	}()

	if len(eps) == 1 {
		ev.Attempts = 1
		out := c.verifyOnce(ctx, eps[0], key, false)
		ev.Endpoint, ev.Result = out.endpoint, resultLabel(out.err)
		return out.result, out.err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan verifyOutcome, len(eps)) // buffered: a late answer never blocks its goroutine
	next, inflight := 0, 0
	launch := func() {
		ep := eps[next]
		// An attempt sent while another is still in flight is a hedge: both
		// may reach a node that would spend a use of a limited key, so the
		// hedge says so and the keystore answers it without claiming.
		hedge := inflight > 0
		next++
		inflight++
		ev.Attempts++
		go func() { results <- c.verifyOnce(ctx, ep, key, hedge) }()
	}
	timer := time.NewTimer(c.hedgeDelay())
	defer timer.Stop()

	launch()
	var lastErr error
	for {
		select {
		case out := <-results:
			inflight--
			if out.definitive() {
				ev.Endpoint, ev.Result = out.endpoint, resultLabel(out.err)
				return out.result, out.err
			}
			lastErr = out.err
			if next < len(eps) {
				ev.Hedged = true
				launch()
				resetTimer(timer, c.hedgeDelay())
				continue
			}
			if inflight == 0 {
				ev.Result = "unavailable"
				return nil, lastErr
			}
		case <-timer.C:
			if next < len(eps) {
				ev.Hedged = true
				launch()
				timer.Reset(c.hedgeDelay())
			}
		case <-ctx.Done():
			ev.Result = "unavailable"
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return nil, wrapUnavailable(lastErr)
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func wrapUnavailable(err error) error {
	if errors.Is(err, ErrKeystoreUnavailable) {
		return err
	}
	return errors.Join(ErrKeystoreUnavailable, err)
}

func resultLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrInvalidKey):
		return "invalid"
	default:
		return "unavailable"
	}
}

// ─── per-endpoint breaker ─────────────────────────────────────────────

// breakers is process-wide and keyed by endpoint URL, so every Client in the
// process shares what it has learnt about a node (and Client stays copyable).
var breakers sync.Map // string -> *breaker

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func breakerFor(ep string) *breaker {
	if b, ok := breakers.Load(ep); ok {
		return b.(*breaker)
	}
	b, _ := breakers.LoadOrStore(ep, &breaker{})
	return b.(*breaker)
}

func (b *breaker) isOpen(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Before(b.openUntil)
}

// record notes the outcome of one request. Only transport-level failures and
// 5xx count against an endpoint; any HTTP answer below 500 proves it is alive.
func (b *breaker) record(ok bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures, b.openUntil = 0, time.Time{}
		return
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = now.Add(breakerCooldown)
	}
}

// resetBreakers is for tests.
func resetBreakers() { breakers = sync.Map{} }

// ─── admin failover ───────────────────────────────────────────────────

// isDialError reports a failure to connect at all: the request never reached
// the server, so retrying it on another node cannot apply it twice.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// ─── environment ──────────────────────────────────────────────────────

// approvedHTTPSSuffixes are the fleet apex domains an HTTPS keystore endpoint
// may live under. A key presented for verification is a credential; an
// endpoint list from the environment must not be able to send it anywhere
// else.
var approvedHTTPSSuffixes = []string{"0exec.com", "0mcp.com", "0own.com", "0docker.com", "0infer.com"}

// cgnat is the Tailscale address range (100.64.0.0/10).
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ParseEndpointList parses APIKEY_SERVICE_URLS: a comma- or space-separated,
// ordered list of keystore base URLs. Entries that are not acceptable are
// dropped (and reported), never used: plain http only to loopback, private
// (RFC 1918), tailnet (100.64/10), host.docker.internal or a single-label
// container name; https only to the fleet's own domains or a private address.
// No credentials, query or fragment; duplicates are removed.
func ParseEndpointList(raw string) (endpoints []string, rejected []string) {
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		u, ok := normalizeEndpoint(f)
		if !ok {
			rejected = append(rejected, f)
			continue
		}
		if !seen[u] {
			seen[u] = true
			endpoints = append(endpoints, u)
		}
	}
	return endpoints, rejected
}

func normalizeEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	privateHost := func() bool {
		if host == "localhost" || host == "host.docker.internal" {
			return true
		}
		if ip := net.ParseIP(host); ip != nil {
			return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
		}
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		// Plain http only on a private network. A single-label name (no dots,
		// not an IP literal) is a container name on a private Docker network.
		singleLabel := net.ParseIP(host) == nil && !strings.Contains(host, ".")
		if !privateHost() && !singleLabel {
			return "", false
		}
	case "https":
		if !privateHost() {
			ok := false
			for _, s := range approvedHTTPSSuffixes {
				if host == s || strings.HasSuffix(host, "."+s) {
					ok = true
				}
			}
			if !ok {
				return "", false
			}
		}
	default:
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, true
}

package proxysupplier

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// webshareDefaultPacingBurst is the bucket size used when WebsharePacing.Burst is zero.
const webshareDefaultPacingBurst = 2

// WebsharePacing configures opt-in, per-ASN request pacing for the
// webshare_direct and webshare_multi suppliers. The zero value disables pacing,
// which leaves selection exactly as it was before this option existed.
//
// Each ASN gets one token bucket (golang.org/x/time/rate). Its refill rate is
// PerIPRate multiplied by the number of that ASN's proxy IPs known to the
// supplier's network-health tracker: for webshare_multi that is the sum over
// every pool sharing the tracker, so two plans that sell IPs from the same ASN
// share its budget, the same way they share its health. A selection takes one
// token from the bucket of the entry it returns and none from the ASNs it
// skipped. When every otherwise-eligible entry's ASN is out of tokens, the
// selection fails with a *PacedError (errors.Is(err, ErrPaced)) instead of
// falling back to an unpaced pick; SelectWait blocks on it for callers that
// would rather wait.
type WebsharePacing struct {
	// PerIPRate is the sustained request rate allowed per proxy IP, in requests
	// per second (for example 1.0/60 for one request per IP per minute). An
	// ASN's bucket refills at PerIPRate × its IP count. Zero, a negative value,
	// NaN or Inf disables pacing; every other field is then ignored.
	PerIPRate float64

	// MinASNRate is a floor, in requests per second, for an ASN's refill rate,
	// so that an ASN with very few IPs is not starved. Zero (or a negative
	// value) means no floor beyond PerIPRate × 1 IP.
	MinASNRate float64

	// Burst is the bucket size: how many requests an idle ASN may serve back to
	// back before the rate applies. Zero (or a negative value) means 2.
	Burst int
}

// Enabled reports whether p turns pacing on (PerIPRate is a positive, finite number).
func (p WebsharePacing) Enabled() bool {
	return p.PerIPRate > 0 && !math.IsInf(p.PerIPRate, 0) && !math.IsNaN(p.PerIPRate)
}

func (p WebsharePacing) normalized() WebsharePacing {
	if !p.Enabled() {
		return WebsharePacing{}
	}
	if p.MinASNRate < 0 || math.IsNaN(p.MinASNRate) || math.IsInf(p.MinASNRate, 0) {
		p.MinASNRate = 0
	}
	if p.Burst <= 0 {
		p.Burst = webshareDefaultPacingBurst
	}
	return p
}

// WebsharePacingFromEnv reads the optional pacing variables
// WEBSHARE_PACING_PER_IP_RATE (requests per second per IP),
// WEBSHARE_PACING_MIN_ASN_RATE (requests per second) and WEBSHARE_PACING_BURST.
// A missing or unparsable value leaves its field at zero, so an unset
// WEBSHARE_PACING_PER_IP_RATE keeps pacing disabled. EnvConfig calls it.
func WebsharePacingFromEnv() WebsharePacing {
	parse := func(name string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0
		}
		return v
	}
	burst, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WEBSHARE_PACING_BURST")))
	if err != nil || burst < 0 {
		burst = 0
	}
	return WebsharePacing{
		PerIPRate:  parse("WEBSHARE_PACING_PER_IP_RATE"),
		MinASNRate: parse("WEBSHARE_PACING_MIN_ASN_RATE"),
		Burst:      burst,
	}
}

// WebsharePacingConfigurer is implemented by the webshare_direct and
// webshare_multi suppliers. SetWebsharePacing replaces the pacing
// configuration at runtime; the zero value turns pacing off. Every bucket
// restarts full when the configuration changes.
type WebsharePacingConfigurer interface {
	SetWebsharePacing(WebsharePacing)
}

// ConfigureWebsharePacing applies p to s when s supports pacing (webshare_direct,
// webshare_multi) and reports whether it did. Suppliers built with
// NewFromConfig already apply Config.WebsharePacing; this is for a supplier
// built another way, or to change the setting at runtime.
func ConfigureWebsharePacing(s Supplier, p WebsharePacing) bool {
	c, ok := s.(WebsharePacingConfigurer)
	if !ok {
		return false
	}
	c.SetWebsharePacing(p)
	return true
}

// ErrPaced is matched (errors.Is) by every *PacedError: a selection found
// eligible proxies, but each of their ASNs had used up its pacing budget.
var ErrPaced = errors.New("proxysupplier: every eligible ASN is paced")

// PacedError is returned by a selection when per-ASN pacing is enabled and no
// eligible entry's ASN has a token. RetryAfter is the shortest wait until at
// least one of those ASNs has a token again. It matches both ErrPaced and
// ErrProxyUnavailable, so callers that only know ErrProxyUnavailable keep
// failing closed.
type PacedError struct {
	RetryAfter time.Duration

	pacer *webshareASNPacer
}

func (e *PacedError) Error() string {
	return fmt.Sprintf("proxysupplier: every eligible ASN is paced, retry after %s", e.RetryAfter)
}

// Is makes errors.Is(err, ErrPaced) and errors.Is(err, ErrProxyUnavailable) true.
func (e *PacedError) Is(target error) bool {
	return target == ErrPaced || target == ErrProxyUnavailable
}

// pacingSleep waits d or until ctx is done, whichever is first, and returns
// ctx.Err(). A var so tests can replace it with a fake clock.
var pacingSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	return ctx.Err()
}

// SelectWait calls selectFn until it returns something other than a
// *PacedError, sleeping min(RetryAfter, time left on ctx) between attempts. It
// returns ctx's error (wrapped together with the last *PacedError, so
// errors.Is matches both) once ctx is done. Any other error is returned at
// once. Typical use with a webshare_multi supplier:
//
//	sel, err := proxysupplier.SelectWait(ctx, func() (proxysupplier.WebshareSelection, error) {
//	    return selector.SelectPool(proxysupplier.PoolClassDC)
//	})
func SelectWait[T any](ctx context.Context, selectFn func() (T, error)) (T, error) {
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		v, err := selectFn()
		var paced *PacedError
		if err == nil || !errors.As(err, &paced) {
			return v, err
		}
		wait := paced.RetryAfter
		if wait <= 0 {
			wait = time.Millisecond
		}
		if deadline, ok := ctx.Deadline(); ok {
			if left := time.Until(deadline); left < wait {
				wait = max(left, 0)
			}
		}
		if paced.pacer != nil {
			paced.pacer.waits.Add(1)
		}
		if err := pacingSleep(ctx, wait); err != nil {
			return zero, fmt.Errorf("%w (%w)", err, paced)
		}
	}
}

// WebsharePacingStatus is the pacing part of WebshareNetworkSnapshot.
type WebsharePacingStatus struct {
	Enabled    bool    `json:"enabled"`
	PerIPRate  float64 `json:"per_ip_rate"`
	MinASNRate float64 `json:"min_asn_rate"`
	Burst      int     `json:"burst"`
	// PacedTotal counts selections that skipped an ASN for lack of a token
	// (one per ASN per selection), summed over every ASN.
	PacedTotal uint64 `json:"paced_total"`
	// WaitsTotal counts the sleeps SelectWait made on this supplier's PacedErrors.
	WaitsTotal uint64 `json:"waits_total"`
}

// webshareASNPacer holds the per-ASN token buckets. It lives inside a
// webshareNetworkHealth, so it is shared exactly as the health tracker is
// (one per webshare_direct supplier, one per webshare_multi supplier).
type webshareASNPacer struct {
	mu      sync.Mutex
	cfg     WebsharePacing // normalized; zero = disabled
	now     func() time.Time
	sources map[*webshareDirectSupplier]map[int]int // catalog -> ASN -> IP count
	ips     map[int]int                             // ASN -> IP count over all sources
	buckets map[int]*rate.Limiter
	paced   map[int]uint64
	total   uint64
	waits   atomic.Uint64
}

func newWebshareASNPacer() *webshareASNPacer {
	return &webshareASNPacer{
		now:     time.Now,
		sources: make(map[*webshareDirectSupplier]map[int]int),
		ips:     make(map[int]int),
		buckets: make(map[int]*rate.Limiter),
		paced:   make(map[int]uint64),
	}
}

func pacingKey(asn int) int {
	if asn <= 0 {
		return 0
	}
	return asn
}

func (p *webshareASNPacer) configure(cfg WebsharePacing) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg.normalized()
	p.buckets = make(map[int]*rate.Limiter)
}

func (p *webshareASNPacer) enabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Enabled()
}

func (p *webshareASNPacer) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

// setSource records how many IPs each ASN has in one catalog and re-rates the
// existing buckets. Buckets of ASNs that left every catalog are dropped.
func (p *webshareASNPacer) setSource(src *webshareDirectSupplier, entries []*webshareDirectEntry) {
	if p == nil || src == nil {
		return
	}
	counts := make(map[int]int)
	for _, e := range entries {
		if e != nil {
			counts[pacingKey(e.asnNumber)]++
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources[src] = counts
	p.ips = make(map[int]int, len(counts))
	for _, c := range p.sources {
		for asn, n := range c {
			p.ips[asn] += n
		}
	}
	now := p.clock()
	for asn, lim := range p.buckets {
		if _, ok := p.ips[asn]; !ok {
			delete(p.buckets, asn)
			continue
		}
		lim.SetLimitAt(now, p.limitLocked(asn))
	}
}

func (p *webshareASNPacer) limitLocked(asn int) rate.Limit {
	ips := p.ips[asn]
	if ips < 1 {
		ips = 1
	}
	r := p.cfg.PerIPRate * float64(ips)
	if r < p.cfg.MinASNRate {
		r = p.cfg.MinASNRate
	}
	return rate.Limit(r)
}

func (p *webshareASNPacer) bucketLocked(asn int) *rate.Limiter {
	lim, ok := p.buckets[asn]
	if !ok {
		lim = rate.NewLimiter(p.limitLocked(asn), p.cfg.Burst)
		p.buckets[asn] = lim
	}
	return lim
}

// waitFor is how long lim needs until it holds one whole token at now.
func waitFor(lim *rate.Limiter, now time.Time) time.Duration {
	tokens := lim.TokensAt(now)
	if tokens >= 1 {
		return 0
	}
	r := float64(lim.Limit())
	if r <= 0 {
		return time.Duration(math.MaxInt64)
	}
	d := time.Duration(math.Ceil((1 - tokens) / r * float64(time.Second)))
	if d < time.Nanosecond {
		d = time.Nanosecond
	}
	return d
}

// peek reports whether asn has a token at now without taking it; when it has
// none, it also returns how long until it will.
func (p *webshareASNPacer) peek(asn int, now time.Time) (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cfg.Enabled() {
		return true, 0
	}
	wait := waitFor(p.bucketLocked(pacingKey(asn)), now)
	return wait == 0, wait
}

// take consumes one token of asn at now if there is one.
func (p *webshareASNPacer) take(asn int, now time.Time) (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cfg.Enabled() {
		return true, 0
	}
	lim := p.bucketLocked(pacingKey(asn))
	if lim.AllowN(now, 1) {
		return true, 0
	}
	return false, waitFor(lim, now)
}

func (p *webshareASNPacer) recordPaced(asn int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := pacingKey(asn)
	if _, ok := p.paced[key]; ok || len(p.paced) < webshareMaxTrackedASNs {
		p.paced[key]++
	}
	p.total++
}

// status returns the configuration, the counters and, per ASN, the paced
// count and the current refill rate (requests per second, 0 when disabled).
func (p *webshareASNPacer) status() (WebsharePacingStatus, map[int]uint64, map[int]float64) {
	if p == nil {
		return WebsharePacingStatus{}, nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := WebsharePacingStatus{
		Enabled:    p.cfg.Enabled(),
		PerIPRate:  p.cfg.PerIPRate,
		MinASNRate: p.cfg.MinASNRate,
		Burst:      p.cfg.Burst,
		PacedTotal: p.total,
		WaitsTotal: p.waits.Load(),
	}
	paced := make(map[int]uint64, len(p.paced))
	for asn, n := range p.paced {
		paced[asn] = n
	}
	var rates map[int]float64
	if st.Enabled {
		rates = make(map[int]float64, len(p.ips))
		for asn := range p.ips {
			rates[asn] = float64(p.limitLocked(asn))
		}
	}
	return st, paced, rates
}

// pacingScan is the pacing state of one selection: the ASNs already found out
// of tokens (each counted once as paced) and the shortest wait among them.
// A nil *pacingScan means pacing is off; every method then allows everything.
type pacingScan struct {
	p       *webshareASNPacer
	now     time.Time
	blocked map[int]time.Duration
}

func (h *webshareNetworkHealth) pacingScan() *pacingScan {
	if h == nil || !h.pacer.enabled() {
		return nil
	}
	return &pacingScan{p: h.pacer, now: h.pacer.clock()}
}

func (ps *pacingScan) block(asn int, wait time.Duration) {
	key := pacingKey(asn)
	if ps.blocked == nil {
		ps.blocked = make(map[int]time.Duration)
	}
	if _, seen := ps.blocked[key]; seen {
		return
	}
	ps.blocked[key] = wait
	ps.p.recordPaced(key)
}

// ready reports whether e's ASN has a token, without taking it.
func (ps *pacingScan) ready(e *webshareDirectEntry) bool {
	if ps == nil {
		return true
	}
	if _, blocked := ps.blocked[pacingKey(e.asnNumber)]; blocked {
		return false
	}
	ok, wait := ps.p.peek(e.asnNumber, ps.now)
	if !ok {
		ps.block(e.asnNumber, wait)
	}
	return ok
}

// take consumes e's ASN token; false when another selection took it first.
func (ps *pacingScan) take(e *webshareDirectEntry) bool {
	if ps == nil {
		return true
	}
	ok, wait := ps.p.take(e.asnNumber, ps.now)
	if !ok {
		ps.block(e.asnNumber, wait)
	}
	return ok
}

// err is the *PacedError for a selection that found nothing, or nil when no
// ASN was skipped for pacing (the selection then simply had no eligible entry).
func (ps *pacingScan) err() *PacedError {
	if ps == nil || len(ps.blocked) == 0 {
		return nil
	}
	shortest := time.Duration(math.MaxInt64)
	for _, w := range ps.blocked {
		if w < shortest {
			shortest = w
		}
	}
	return &PacedError{RetryAfter: shortest, pacer: ps.p}
}

// shorterPaced keeps the *PacedError with the shorter RetryAfter.
func shorterPaced(a, b *PacedError) *PacedError {
	if a == nil {
		return b
	}
	if b == nil || a.RetryAfter <= b.RetryAfter {
		return a
	}
	return b
}

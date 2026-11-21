package proxysupplier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// near reports whether got is within 1µs of want (float rounding in the bucket).
func near(got, want time.Duration) bool {
	d := got - want
	return d >= -time.Microsecond && d <= time.Microsecond
}

// fakeClock is a manually advanced clock for the pacer.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// pacedTestMulti is a one-pool multi supplier: AS100 with 4 IPs, AS200 with 2.
func pacedTestMulti(t *testing.T, p WebsharePacing) (*webshareMultiSupplier, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	h := newWebshareNetworkHealth()
	h.pacer.now = clk.Now
	pool := supplierWithHealth(h,
		asnEntry("10.1.1.1", 100), asnEntry("10.2.1.1", 200), asnEntry("10.1.2.1", 100),
		asnEntry("10.2.2.1", 200), asnEntry("10.1.3.1", 100), asnEntry("10.1.4.1", 100))
	m := &webshareMultiSupplier{networkHealth: h, rules: parseNoProxy(""), pools: []*webshareNamedPool{
		{WebsharePoolConfig: WebsharePoolConfig{ID: "dc", AccountID: "a", Class: PoolClassDC}, supplier: pool},
	}}
	m.SetWebsharePacing(p)
	return m, clk
}

// drain selects until the first error and returns how many selections each ASN served.
func drain(t *testing.T, m *webshareMultiSupplier) (map[int]int, error) {
	t.Helper()
	got := make(map[int]int)
	for i := 0; i < 10_000; i++ {
		sel, err := m.SelectPool(PoolClassDC)
		if err != nil {
			return got, err
		}
		got[sel.ASNNumber]++
	}
	t.Fatal("pacing never stopped the selections")
	return nil, nil
}

func TestWebsharePacing_DisabledIsIdenticalToNoPacing(t *testing.T) {
	build := func() *webshareDirectSupplier {
		h := newWebshareNetworkHealth()
		degradeASN(h, 200, 7, 3, time.Now())
		var list []*webshareDirectEntry
		for i := 1; i <= 5; i++ {
			list = append(list, asnEntry(fmt.Sprintf("10.1.%d.1", i), 100), asnEntry(fmt.Sprintf("10.2.%d.1", i), 200))
		}
		return supplierWithHealth(h, interleaveWebshareEntries(list)...)
	}
	never := build()
	zero := build()
	zero.SetWebsharePacing(WebsharePacing{})
	toggled := build()
	toggled.SetWebsharePacing(WebsharePacing{PerIPRate: 1})
	toggled.SetWebsharePacing(WebsharePacing{PerIPRate: 0, MinASNRate: 5, Burst: 9}) // PerIPRate 0: off

	for i := 0; i < 2_000; i++ {
		a, errA := never.selectEntry(nil)
		b, errB := zero.selectEntry(nil)
		c, errC := toggled.selectEntry(nil)
		if errA != nil || errB != nil || errC != nil {
			t.Fatalf("pick %d: errors %v %v %v", i, errA, errB, errC)
		}
		if a.addr != b.addr || a.addr != c.addr {
			t.Fatalf("pick %d differs: %s / %s / %s", i, a.addr, b.addr, c.addr)
		}
	}
	if st := zero.WebshareNetworkHealth().Pacing; st.Enabled || st.PacedTotal != 0 {
		t.Fatalf("disabled pacing reported %+v", st)
	}
}

func TestWebsharePacing_RespectsPerASNRateOverAFakeClock(t *testing.T) {
	m, clk := pacedTestMulti(t, WebsharePacing{PerIPRate: 1, Burst: 2})
	// AS100: 4 IPs -> 4/s; AS200: 2 IPs -> 2/s. Simulate 10 s in 10 ms steps.
	total := make(map[int]int)
	for step := 0; step < 1000; step++ {
		got, err := drain(t, m)
		if !errors.Is(err, ErrPaced) {
			t.Fatalf("step %d: error %v, want ErrPaced", step, err)
		}
		for asn, n := range got {
			total[asn] += n
		}
		clk.Advance(10 * time.Millisecond)
	}
	// Burst (2) + rate x 10 s, with one step of slack for rounding.
	if total[100] < 41 || total[100] > 43 {
		t.Errorf("AS100 served %d in 10 s, want 42 (4/s + burst 2)", total[100])
	}
	if total[200] < 21 || total[200] > 23 {
		t.Errorf("AS200 served %d in 10 s, want 22 (2/s + burst 2)", total[200])
	}
	snap := m.WebshareNetworkHealth()
	if !snap.Pacing.Enabled || snap.Pacing.PerIPRate != 1 || snap.Pacing.Burst != 2 || snap.Pacing.PacedTotal == 0 {
		t.Fatalf("pacing status = %+v", snap.Pacing)
	}
	for _, a := range snap.ASNs {
		want := map[int]float64{100: 4, 200: 2}[a.ASNNumber]
		if a.PacingRatePerSecond != want || a.PacedTotal == 0 {
			t.Errorf("AS%d status: rate %.1f paced %d, want rate %.1f and paced > 0", a.ASNNumber, a.PacingRatePerSecond, a.PacedTotal, want)
		}
	}
}

func TestWebsharePacing_NoUnpacedFallbackAndRetryAfter(t *testing.T) {
	m, clk := pacedTestMulti(t, WebsharePacing{PerIPRate: 1, Burst: 2})
	got, err := drain(t, m)
	if got[100] != 2 || got[200] != 2 {
		t.Fatalf("first burst served %v, want 2 per ASN", got)
	}
	var paced *PacedError
	if !errors.As(err, &paced) || !errors.Is(err, ErrPaced) || !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("error %v: want a *PacedError matching ErrPaced and ErrProxyUnavailable", err)
	}
	// Both buckets are empty: AS100 refills at 4/s, so the shortest wait is 250 ms.
	if !near(paced.RetryAfter, 250*time.Millisecond) {
		t.Fatalf("RetryAfter = %s, want 250ms", paced.RetryAfter)
	}
	// Still no fallback: every path returns nothing until a token arrives.
	for i := 0; i < 20; i++ {
		if u := m.ProxyURL(); u != "" {
			t.Fatalf("ProxyURL fell back to %q while every ASN was paced", u)
		}
		if _, err := m.NextProxyURL(); !errors.Is(err, ErrPaced) {
			t.Fatalf("NextProxyURL error %v, want ErrPaced", err)
		}
	}
	clk.Advance(100 * time.Millisecond)
	_, err = m.SelectPool(PoolClassDC)
	if !errors.As(err, &paced) {
		t.Fatalf("after 100ms: %v, want paced", err)
	}
	if !near(paced.RetryAfter, 150*time.Millisecond) {
		t.Fatalf("RetryAfter after 100ms = %s, want 150ms", paced.RetryAfter)
	}
	clk.Advance(151 * time.Millisecond)
	sel, err := m.SelectPool(PoolClassDC)
	if err != nil || sel.ASNNumber != 100 {
		t.Fatalf("after RetryAfter: AS%d, %v; want AS100", sel.ASNNumber, err)
	}
}

func TestWebsharePacing_MinASNRateFloorAndSharedAcrossPools(t *testing.T) {
	clk := newFakeClock()
	h := newWebshareNetworkHealth()
	h.pacer.now = clk.Now
	a := supplierWithHealth(h, asnEntry("10.1.1.1", 100), asnEntry("10.1.2.1", 100), asnEntry("10.3.1.1", 300))
	b := supplierWithHealth(h, asnEntry("10.1.3.1", 100), asnEntry("10.1.4.1", 100))
	m := &webshareMultiSupplier{networkHealth: h, pools: []*webshareNamedPool{
		{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy-dc", AccountID: "a", Class: PoolClassDC}, supplier: a},
		{WebsharePoolConfig: WebsharePoolConfig{ID: "quality-dc", AccountID: "b", Class: PoolClassDC}, supplier: b},
	}}
	m.SetWebsharePacing(WebsharePacing{PerIPRate: 0.1, MinASNRate: 0.25, Burst: 1})
	rates := make(map[int]float64)
	for _, st := range m.WebshareNetworkHealth().ASNs {
		rates[st.ASNNumber] = st.PacingRatePerSecond
	}
	// AS100 has 2 IPs in each pool: 4 x 0.1 = 0.4/s. AS300 has 1: 0.1, floored at 0.25.
	if rates[100] < 0.3999 || rates[100] > 0.4001 || rates[300] != 0.25 {
		t.Fatalf("rates = %v, want AS100 0.4 and AS300 0.25", rates)
	}
	if got, _ := drain(t, m); got[100] != 1 || got[300] != 1 {
		t.Fatalf("burst 1 served %v, want one per ASN across both pools", got)
	}
	_, err := m.SelectPool(PoolClassDC)
	var paced *PacedError
	if !errors.As(err, &paced) || !near(paced.RetryAfter, 2500*time.Millisecond) {
		t.Fatalf("err %v: want RetryAfter 2.5s (AS100 at 0.4/s)", err)
	}
	// A refresh that drops the pool b's IPs re-rates AS100 to 2 x 0.1 = 0.2, floored to 0.25.
	h.pacer.setSource(b, nil)
	for _, st := range m.WebshareNetworkHealth().ASNs {
		if st.ASNNumber == 100 && st.PacingRatePerSecond != 0.25 {
			t.Fatalf("AS100 rate after re-rate = %v, want 0.25", st.PacingRatePerSecond)
		}
	}
}

// A paced selection must not take tokens from the ASNs it skipped, and must
// not fall back onto a quarantined ASN while a healthy one only needs to wait.
func TestWebsharePacing_SkippedASNsKeepTheirTokens(t *testing.T) {
	clk := newFakeClock()
	h := newWebshareNetworkHealth()
	h.pacer.now = clk.Now
	degradeASN(h, 100, 0, 10, time.Now()) // AS100 quarantined
	s := supplierWithHealth(h, asnEntry("10.1.1.1", 100), asnEntry("10.2.1.1", 200), asnEntry("10.1.2.1", 100))
	s.SetWebsharePacing(WebsharePacing{PerIPRate: 1, Burst: 2})
	for i := 0; i < 2; i++ {
		e, err := s.selectEntry(nil)
		if err != nil || e.asnNumber != 200 {
			t.Fatalf("pick %d: %v %v; want AS200", i, e, err)
		}
	}
	e, err := s.selectEntry(nil)
	if !errors.Is(err, ErrPaced) {
		t.Fatalf("third pick = %v, %v; want ErrPaced, not a fallback onto quarantined AS100", e, err)
	}
	if ok, _ := h.pacer.peek(100, clk.Now()); !ok {
		t.Fatal("the skipped (quarantined) AS100 lost tokens")
	}
}

func TestSelectWait_SleepsRetryAfterAndRetries(t *testing.T) {
	m, clk := pacedTestMulti(t, WebsharePacing{PerIPRate: 1, Burst: 2})
	if _, err := drain(t, m); !errors.Is(err, ErrPaced) {
		t.Fatal(err)
	}
	var slept []time.Duration
	orig := pacingSleep
	pacingSleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		clk.Advance(d)
		return ctx.Err()
	}
	t.Cleanup(func() { pacingSleep = orig })

	sel, err := SelectWait(context.Background(), func() (WebshareSelection, error) { return m.SelectPool(PoolClassDC) })
	if err != nil || sel.ASNNumber != 100 {
		t.Fatalf("SelectWait = AS%d, %v", sel.ASNNumber, err)
	}
	if len(slept) != 1 || !near(slept[0], 250*time.Millisecond) {
		t.Fatalf("slept %v, want one 250ms sleep", slept)
	}
	if w := m.WebshareNetworkHealth().Pacing.WaitsTotal; w != 1 {
		t.Fatalf("WaitsTotal = %d, want 1", w)
	}
	// NextProxyURL works with it directly.
	clk.Advance(time.Hour)
	if u, err := SelectWait(context.Background(), m.NextProxyURL); err != nil || u == "" {
		t.Fatalf("SelectWait(NextProxyURL) = %q, %v", u, err)
	}
	// Errors other than pacing return at once.
	calls := 0
	_, err = SelectWait(context.Background(), func() (int, error) { calls++; return 0, ErrProxyUnavailable })
	if !errors.Is(err, ErrProxyUnavailable) || errors.Is(err, ErrPaced) || calls != 1 {
		t.Fatalf("non-paced error: %v after %d calls", err, calls)
	}
}

func TestSelectWait_HonoursContext(t *testing.T) {
	calls := 0
	paced := func() (string, error) { calls++; return "", &PacedError{RetryAfter: time.Hour} }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := SelectWait(ctx, paced)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SelectWait ignored the deadline: %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrPaced) {
		t.Fatalf("err = %v, want DeadlineExceeded wrapped with ErrPaced", err)
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	calls = 0
	if _, err := SelectWait(canceled, paced); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled ctx: err %v after %d selections, want Canceled and none", err, calls)
	}

	// A cancel during the sleep wakes it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel2() }()
	if _, err := SelectWait(ctx2, paced); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during sleep: %v", err)
	}
}

func TestWebsharePacing_ConfigAndEnv(t *testing.T) {
	t.Setenv("WEBSHARE_PACING_PER_IP_RATE", "0.05")
	t.Setenv("WEBSHARE_PACING_MIN_ASN_RATE", "0.5")
	t.Setenv("WEBSHARE_PACING_BURST", "3")
	if got := EnvConfig().WebsharePacing; got != (WebsharePacing{PerIPRate: 0.05, MinASNRate: 0.5, Burst: 3}) {
		t.Fatalf("EnvConfig pacing = %+v", got)
	}
	t.Setenv("WEBSHARE_PACING_PER_IP_RATE", "fast")
	t.Setenv("WEBSHARE_PACING_BURST", "-1")
	if got := WebsharePacingFromEnv(); got.Enabled() || got.Burst != 0 {
		t.Fatalf("invalid env pacing = %+v, want disabled", got)
	}
	if got := (WebsharePacing{PerIPRate: 2}).normalized(); got.Burst != webshareDefaultPacingBurst {
		t.Fatalf("default burst = %d", got.Burst)
	}

	s := &webshareDirectSupplier{networkHealth: newWebshareNetworkHealth()}
	if !ConfigureWebsharePacing(s, WebsharePacing{PerIPRate: 1}) || !s.networkHealth.pacer.enabled() {
		t.Fatal("ConfigureWebsharePacing did not enable pacing on webshare_direct")
	}
	if ConfigureWebsharePacing(noneSupplier{}, WebsharePacing{PerIPRate: 1}) {
		t.Fatal("ConfigureWebsharePacing claimed to configure the none supplier")
	}

	// NewFromConfig applies Config.WebsharePacing to webshare_multi.
	dir := t.TempDir()
	path := writePoolConfig(t, poolConfig(t, dir, "dc", "acct", PoolClassDC, "1", 0))
	withWebshareDirectBaseURL(t, "http://127.0.0.1:1")
	sup := NewFromConfig(Config{Supplier: "webshare_multi", WebsharePoolsFile: path, WebsharePacing: WebsharePacing{PerIPRate: 0.5}})
	m, ok := sup.(*webshareMultiSupplier)
	if !ok {
		t.Fatalf("supplier = %T", sup)
	}
	defer m.Close()
	if st := m.WebshareNetworkHealth().Pacing; !st.Enabled || st.PerIPRate != 0.5 || st.Burst != 2 {
		t.Fatalf("multi pacing status = %+v", st)
	}
	for _, p := range m.pools {
		if p.supplier.networkHealth != m.networkHealth {
			t.Fatal("pool does not share the multi supplier's health tracker")
		}
	}
}

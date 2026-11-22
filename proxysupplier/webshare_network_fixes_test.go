package proxysupplier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// asnEntry builds a pool entry in asn with a /24 derived from ip.
func asnEntry(ip string, asn int) *webshareDirectEntry {
	return &webshareDirectEntry{
		addr: ip + ":80", proxyURL: "http://u:p@" + ip + ":80",
		asnNumber: asn, asnName: fmt.Sprintf("Net%d", asn), subnet24: subnet24ForHost(ip),
	}
}

// supplierWithHealth installs entries (in the given order) on a supplier that
// uses h, and registers them with h's pacer as a catalog refresh would.
func supplierWithHealth(h *webshareNetworkHealth, entries ...*webshareDirectEntry) *webshareDirectSupplier {
	s := &webshareDirectSupplier{networkHealth: h}
	installEntries(s, entries...)
	h.pacer.setSource(s, entries)
	return s
}

// degradeASN feeds asn oks then fails so its rolling failure ratio is
// fails/(oks+fails) without touching any /24 (the subnet is left empty).
func degradeASN(h *webshareNetworkHealth, asn, oks, fails int, now time.Time) {
	for i := 0; i < oks; i++ {
		h.Record(asn, "", "", "", true, now)
	}
	for i := 0; i < fails; i++ {
		h.Record(asn, "", "", "", false, now)
	}
}

// The refresh used to write asnNumber, asnName, subnet24 and countryCode of
// entries already published in s.list while selection read them without the
// lock. Under -race this test failed; now changed metadata produces a new
// entry, and the per-IP state is carried over.
func TestWebshareDirect_RefreshDoesNotWritePublishedEntries(t *testing.T) {
	var flip atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := flip.Add(1)
		results := make([]webshareEntry, 0, 8)
		for i := 0; i < 8; i++ {
			results = append(results, webshareEntry{
				Username: "u", Password: "p", ProxyAddress: fmt.Sprintf("10.9.%d.%d", i%2, i+1), Port: 80, Valid: true,
				ASNNumber: 100 + int(n%2), ASNName: fmt.Sprintf("Net-%d", n%2), CountryCode: "US",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(results), Results: results})
	}))
	defer srv.Close()

	s := directSupplierForTest("test-key", "")
	s.baseURL = srv.URL + "/"
	s.networkHealth = newWebshareNetworkHealth()
	s.networkHealth.pacer.configure(WebsharePacing{PerIPRate: 1000, Burst: 1000})
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Cool one entry down, then change its metadata: the state must survive.
	first := s.nextEntry()
	s.MarkResult(first.addr, false)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if e := s.nextEntry(); e != nil {
					_ = e.asnNumber + len(e.asnName) + len(e.subnet24) + len(e.countryCode)
					s.MarkOutcome(e.addr, OutcomeOK)
				}
				_ = s.WebshareNetworkHealth()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := s.refresh(context.Background()); err != nil {
			t.Errorf("refresh %d: %v", i, err)
		}
	}
	cancel()
	wg.Wait()

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.list {
		if e.asnNumber != 100+int(flip.Load()%2) {
			t.Fatalf("entry %s kept ASN %d after a refresh that changed it", e.addr, e.asnNumber)
		}
	}
}

func TestWebshareDirect_RefreshWithNewMetadataCarriesCooldown(t *testing.T) {
	asn := atomic.Int64{}
	asn.Store(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := []webshareEntry{
			{Username: "u", Password: "p", ProxyAddress: "10.9.0.1", Port: 80, Valid: true, ASNNumber: int(asn.Load())},
			{Username: "u", Password: "p", ProxyAddress: "10.9.1.1", Port: 80, Valid: true, ASNNumber: 300},
		}
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(results), Results: results})
	}))
	defer srv.Close()
	s := directSupplierForTest("test-key", "")
	s.baseURL = srv.URL + "/"
	s.networkHealth = newWebshareNetworkHealth()
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.MarkResult("10.9.0.1:80", false)
	s.mu.RLock()
	var before, unchanged *webshareDirectEntry
	for _, e := range s.list {
		switch e.addr {
		case "10.9.0.1:80":
			before = e
		case "10.9.1.1:80":
			unchanged = e
		}
	}
	s.mu.RUnlock()

	asn.Store(200)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.list {
		switch e.addr {
		case "10.9.0.1:80":
			if e == before {
				t.Fatal("an entry whose ASN changed was written in place")
			}
			if e.asnNumber != 200 || e.failures.Load() != before.failures.Load() || e.cooldownUntil.Load() != before.cooldownUntil.Load() || e.cooldownUntil.Load() == 0 {
				t.Fatalf("new entry did not carry the cooldown: asn=%d failures=%d cooldown=%d", e.asnNumber, e.failures.Load(), e.cooldownUntil.Load())
			}
			if before.asnNumber != 100 {
				t.Fatalf("published entry was mutated to AS%d", before.asnNumber)
			}
		case "10.9.1.1:80":
			if e != unchanged {
				t.Fatal("an entry with unchanged metadata was replaced; in-flight selections would lose their feedback")
			}
		}
	}
}

func TestWebshareNetworkHealth_PrunesIdleSubnets(t *testing.T) {
	origWindow := webshareSubnetWindow
	webshareSubnetWindow = time.Minute
	t.Cleanup(func() { webshareSubnetWindow = origWindow })

	h := newWebshareNetworkHealth()
	t0 := time.Unix(1_700_000_000, 0)
	// One cooling /24 (two distinct IPs failed) and a full map of single failures.
	h.Record(1, "", "10.0.0.0/24", "10.0.0.1:80", false, t0)
	h.Record(1, "", "10.0.0.0/24", "10.0.0.2:80", false, t0)
	for i := 1; len(h.subnets) < webshareMaxTrackedSubnets; i++ {
		h.Record(1, "", fmt.Sprintf("10.%d.%d.0/24", 1+i/256, i%256), "x:80", false, t0)
	}
	h.Record(1, "", "172.16.0.0/24", "172.16.0.1:80", false, t0.Add(time.Second))
	if _, ok := h.subnets["172.16.0.0/24"]; ok {
		t.Fatal("a full map of fresh failures must not take a new subnet")
	}

	// Once the window has passed, the full map is pruned and the new /24 is tracked.
	later := t0.Add(2 * time.Minute)
	h.Record(1, "", "172.16.1.0/24", "172.16.1.1:80", false, later)
	if _, ok := h.subnets["172.16.1.0/24"]; !ok {
		t.Fatal("new subnet still untracked after the old failures left the window")
	}
	if _, ok := h.subnets["10.0.0.0/24"]; !ok {
		t.Fatal("a subnet that is still cooling down was pruned")
	}
	if len(h.subnets) != 2 {
		t.Fatalf("tracked subnets = %d, want 2 (the cooling one and the new one)", len(h.subnets))
	}

	// After the cooldown too, PruneSubnets (run on every refresh) empties the map.
	if n := h.PruneSubnets(t0.Add(webshareSubnetCooldown + 3*time.Minute)); n != 2 || len(h.subnets) != 0 {
		t.Fatalf("PruneSubnets pruned %d, %d left; want 2 and 0", n, len(h.subnets))
	}

	// A success that leaves a subnet idle drops it at once.
	h.Record(1, "", "10.7.7.0/24", "10.7.7.1:80", false, later)
	h.Record(1, "", "10.7.7.0/24", "10.7.7.1:80", true, later.Add(time.Second))
	if _, ok := h.subnets["10.7.7.0/24"]; ok {
		t.Fatal("an idle subnet survived the success that cleared it")
	}
}

// For a degraded ASN the old thinning tested the parity of the cursor, which
// in an even-sized list equals the parity of the entry's position: the ASN's
// IPs at odd positions were never admitted. Admission now counts per entry.
func TestWebshareDirect_DegradedThinningRotatesOverEveryIP(t *testing.T) {
	for _, tc := range []struct {
		name        string
		oks, fails  int
		wantShareLo float64
		wantShareHi float64
	}{
		{"degraded half", 7, 3, 0.15, 0.35},     // 1 in 2 considerations
		{"heavily degraded", 11, 9, 0.05, 0.20}, // 1 in 4 considerations
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWebshareNetworkHealth()
			degradeASN(h, 100, tc.oks, tc.fails, time.Now())
			var list []*webshareDirectEntry
			for i := 1; i <= 4; i++ {
				list = append(list, asnEntry(fmt.Sprintf("10.2.%d.1", i), 200), asnEntry(fmt.Sprintf("10.1.%d.1", i), 100))
			}
			s := supplierWithHealth(h, list...)
			counts := make(map[string]int)
			const picks = 1600
			degraded := 0
			for i := 0; i < picks; i++ {
				e := s.nextEntry()
				if e == nil {
					t.Fatal("no entry selected")
				}
				counts[e.addr]++
				if e.asnNumber == 100 {
					degraded++
				}
			}
			for i := 1; i <= 4; i++ {
				addr := fmt.Sprintf("10.1.%d.1:80", i)
				if counts[addr] < 20 {
					t.Errorf("degraded IP %s picked %d times out of %d; every IP must keep rotating", addr, counts[addr], picks)
				}
			}
			share := float64(degraded) / picks
			if share < tc.wantShareLo || share > tc.wantShareHi {
				t.Errorf("degraded ASN share = %.3f, want %.2f..%.2f", share, tc.wantShareLo, tc.wantShareHi)
			}
		})
	}
}

func TestWebshareOutcomeKinds_OnlyChallengesCountAgainstTheNetwork(t *testing.T) {
	h := newWebshareNetworkHealth()
	var list []*webshareDirectEntry
	for i := 1; i <= 12; i++ {
		list = append(list, asnEntry(fmt.Sprintf("10.1.1.%d", i), 100))
	}
	s := supplierWithHealth(h, list...)
	asn100 := func() WebshareASNStatus {
		snap := s.WebshareNetworkHealth()
		for _, a := range snap.ASNs {
			if a.ASNNumber == 100 {
				return a
			}
		}
		t.Fatal("AS100 missing from snapshot")
		return WebshareASNStatus{}
	}

	// Transport and upstream failures cool the exit IP but leave the ASN and /24 alone.
	for i, e := range list {
		kind := OutcomeTransport
		if i%2 == 1 {
			kind = OutcomeUpstream
		}
		s.MarkOutcome(e.addr, kind)
	}
	now := time.Now().UnixNano()
	for _, e := range list {
		if e.eligible(now) {
			t.Fatalf("%s still eligible after a transport/upstream failure; the per-IP cooldown must not change", e.addr)
		}
	}
	if a := asn100(); a.FailTotal != 0 || a.Samples != 0 || a.State != WebshareASNStateHealthy || a.CooledSubnets24 != 0 {
		t.Fatalf("transport/upstream outcomes counted against the network: %+v", a)
	}

	// OK clears the exit IP and counts as ok.
	s.MarkOutcome(list[0].addr, OutcomeOK)
	if !list[0].eligible(time.Now().UnixNano()) {
		t.Fatal("OutcomeOK did not clear the exit IP")
	}
	if a := asn100(); a.OKTotal != 1 {
		t.Fatalf("OutcomeOK not counted: %+v", a)
	}

	// Challenges count: two IPs in the /24 cool it, and enough of them quarantine the ASN.
	for i := 0; i < webshareASNMinSamples; i++ {
		s.MarkOutcome(list[i%10].addr, OutcomeChallenge)
	}
	if a := asn100(); a.FailTotal != uint64(webshareASNMinSamples) || a.State != WebshareASNStateQuarantined || a.CooledSubnets24 != 1 {
		t.Fatalf("challenges not counted: %+v", a)
	}

	// Unknown outcomes are ignored entirely.
	before := asn100()
	s.MarkOutcome(list[11].addr, Outcome(0))
	s.MarkOutcome(list[11].addr, Outcome(200))
	if after := asn100(); after.OKTotal != before.OKTotal || after.FailTotal != before.FailTotal {
		t.Fatalf("unknown outcome changed health: %+v -> %+v", before, after)
	}
}

func TestWebshareMulti_SelectionOutcomeAndLegacyBool(t *testing.T) {
	shared := newWebshareNetworkHealth()
	p := supplierWithHealth(shared, asnEntry("10.1.1.1", 100), asnEntry("10.1.2.1", 100))
	m := &webshareMultiSupplier{networkHealth: shared, pools: []*webshareNamedPool{
		{WebsharePoolConfig: WebsharePoolConfig{ID: "dc", AccountID: "a", Class: PoolClassDC}, supplier: p},
	}}
	sel, err := m.SelectPool(PoolClassDC)
	if err != nil {
		t.Fatal(err)
	}
	m.MarkSelectionOutcome(sel, OutcomeTransport)
	if sel.entry.eligible(time.Now().UnixNano()) {
		t.Fatal("transport failure did not cool the selected IP")
	}
	if snap := m.WebshareNetworkHealth(); snap.ASNs[0].FailTotal != 0 {
		t.Fatalf("transport failure counted for the ASN: %+v", snap.ASNs[0])
	}
	// The bool API keeps its old meaning: false is a network failure.
	m.MarkSelectionResult(sel, false)
	m.MarkSelectionOutcome(sel, OutcomeChallenge)
	m.MarkOutcome(sel.entry.addr, OutcomeChallenge)
	m.MarkResult(sel.entry.addr, true)
	if snap := m.WebshareNetworkHealth(); snap.ASNs[0].FailTotal != 3 || snap.ASNs[0].OKTotal != 1 {
		t.Fatalf("want 3 failures and 1 ok, got %+v", snap.ASNs[0])
	}
}

// Passes 2 and 3 used to ignore excludeASNs, so a retry that excluded the ASN
// that had just challenged it landed right back on it whenever thinning
// skipped the other ASN's entry, or whenever the pool picked first had only
// the excluded ASN.
func TestWebshareMulti_RetryAvoidsExcludedASNWhileAnotherExists(t *testing.T) {
	shared := newWebshareNetworkHealth()
	degradeASN(shared, 200, 7, 3, time.Now()) // AS200 is degraded: pass 1 thins it
	legacy := supplierWithHealth(shared, asnEntry("10.1.1.1", 100), asnEntry("10.2.1.1", 200), asnEntry("10.1.2.1", 100))
	quality := supplierWithHealth(shared, asnEntry("10.1.3.1", 100), asnEntry("10.1.4.1", 100))
	m := &webshareMultiSupplier{networkHealth: shared, pools: []*webshareNamedPool{
		{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy-dc", AccountID: "legacy", Class: PoolClassDC}, supplier: legacy},
		{WebsharePoolConfig: WebsharePoolConfig{ID: "quality-dc", AccountID: "quality", Class: PoolClassDC}, supplier: quality},
	}}
	for i := 0; i < 200; i++ {
		sel, err := m.SelectPoolExcludingASNs(PoolClassDC, nil, []int{100})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		if sel.ASNNumber == 100 {
			t.Fatalf("select %d returned excluded AS100 from pool %s while AS200 was available", i, sel.PoolID)
		}
	}
	// The named-pool form honours it too.
	for i := 0; i < 50; i++ {
		sel, err := m.SelectPoolIDExcludingASNs("legacy-dc", []int{100})
		if err != nil || sel.ASNNumber != 200 {
			t.Fatalf("SelectPoolIDExcludingASNs = AS%d, %v; want AS200", sel.ASNNumber, err)
		}
	}
	// With no other ASN left, the excluded one is still the last resort.
	sel, err := m.SelectPoolIDExcludingASNs("quality-dc", []int{100})
	if err != nil || sel.ASNNumber != 100 {
		t.Fatalf("last resort = AS%d, %v; want AS100", sel.ASNNumber, err)
	}
	if _, err := m.SelectPoolExcludingASNs(PoolClassDC, nil, []int{100, 200}); err != nil {
		t.Fatalf("excluding every ASN must still fall back, got %v", err)
	}
}

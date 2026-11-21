package proxysupplier

import (
	"fmt"
	"testing"
	"time"
)

// sixASNMulti is a two-pool multi supplier over six ASNs (101..106) with
// three IPs each, interleaved as a catalog refresh would. ASN 102's IPs share
// one /24; the others have a /24 per IP. Pool "legacy-dc" holds ASNs
// 101..104, "quality-dc" holds 105 and 106.
func sixASNMulti(t *testing.T) (*webshareMultiSupplier, *webshareNetworkHealth) {
	t.Helper()
	h := newWebshareNetworkHealth()
	var legacy, quality []*webshareDirectEntry
	for asn := 101; asn <= 106; asn++ {
		for i := 1; i <= 3; i++ {
			ip := fmt.Sprintf("10.%d.%d.1", asn-100, i)
			if asn == 102 {
				ip = fmt.Sprintf("10.2.0.%d", i)
			}
			e := asnEntry(ip, asn)
			if asn <= 104 {
				legacy = append(legacy, e)
			} else {
				quality = append(quality, e)
			}
		}
	}
	m := &webshareMultiSupplier{networkHealth: h, pools: []*webshareNamedPool{
		{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy-dc", AccountID: "legacy", Class: PoolClassDC}, supplier: supplierWithHealth(h, interleaveWebshareEntries(legacy)...)},
		{WebsharePoolConfig: WebsharePoolConfig{ID: "quality-dc", AccountID: "quality", Class: PoolClassDC}, supplier: supplierWithHealth(h, interleaveWebshareEntries(quality)...)},
	}}
	return m, h
}

// After several ASNs are quarantined, the ASNs that were never tried (no
// health samples) stay selectable, count as healthy capacity, and take the
// traffic in even shares.
func TestWebshareMulti_UntriedASNsTakeOverWhenOthersAreQuarantined(t *testing.T) {
	m, h := sixASNMulti(t)
	if c := m.HealthyCapacityAll(); c != (ASNCapacity{HealthyASNs: 6, EligibleIPs: 18}) {
		t.Fatalf("fresh capacity = %+v, want 6 ASNs / 18 IPs: untried ASNs are healthy", c)
	}

	now := time.Now()
	for _, asn := range []int{101, 103, 105} {
		degradeASN(h, asn, 0, 10, now)
	}
	c := m.HealthyCapacityAll()
	if c.HealthyASNs != 3 || c.EligibleIPs != 9 || c.QuarantinedASNs != 3 {
		t.Fatalf("capacity = %+v, want 3 healthy ASNs, 9 IPs, 3 quarantined", c)
	}
	if c.NextProbeIn < webshareASNBaseCooldown-time.Minute || c.NextProbeIn > webshareASNBaseCooldown {
		t.Fatalf("NextProbeIn = %s, want about %s", c.NextProbeIn, webshareASNBaseCooldown)
	}

	counts := make(map[int]int)
	const picks = 600
	for i := 0; i < picks; i++ {
		sel, err := m.SelectPool(PoolClassDC)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		counts[sel.ASNNumber]++
	}
	for _, asn := range []int{101, 103, 105} {
		if counts[asn] != 0 {
			t.Errorf("quarantined AS%d picked %d times", asn, counts[asn])
		}
	}
	for _, asn := range []int{102, 104, 106} {
		if share := float64(counts[asn]) / picks; share < 0.15 {
			t.Errorf("untried AS%d got %.1f%% of picks; selection must spread over the untried ASNs (%v)", asn, 100*share, counts)
		}
	}
}

func TestWebshareMulti_HealthyCapacityPerPoolAndIPAndSubnetCooldowns(t *testing.T) {
	m, h := sixASNMulti(t)
	degradeASN(h, 105, 0, 10, time.Now())

	if c := m.HealthyCapacity("quality-dc"); c.HealthyASNs != 1 || c.EligibleIPs != 3 || c.QuarantinedASNs != 1 {
		t.Fatalf("quality-dc capacity = %+v, want AS106 only", c)
	}
	if c := m.HealthyCapacity("legacy-dc"); c != (ASNCapacity{HealthyASNs: 4, EligibleIPs: 12}) {
		t.Fatalf("legacy-dc capacity = %+v", c)
	}
	if c := m.HealthyCapacity("no-such-pool"); c != (ASNCapacity{}) {
		t.Fatalf("unknown pool capacity = %+v, want zero", c)
	}

	// A per-IP cooldown (a transport error) removes only that IP.
	m.MarkOutcome("10.1.1.1:80", OutcomeTransport)
	if c := m.HealthyCapacity("legacy-dc"); c.HealthyASNs != 4 || c.EligibleIPs != 11 {
		t.Fatalf("after one IP cooldown: %+v, want 4 ASNs / 11 IPs", c)
	}
	// Two challenged IPs in AS102's single /24 cool the /24: AS102 has nothing left.
	m.MarkOutcome("10.2.0.1:80", OutcomeChallenge)
	m.MarkOutcome("10.2.0.2:80", OutcomeChallenge)
	c := m.HealthyCapacity("legacy-dc")
	if c.HealthyASNs != 3 || c.EligibleIPs != 8 || c.QuarantinedASNs != 0 {
		t.Fatalf("after /24 cooldown: %+v, want 3 ASNs / 8 IPs", c)
	}

	// A probe that was just admitted pushes the next one out by the probe interval.
	ah := h.asns[105]
	h.mu.Lock()
	ah.cooldownUntil = time.Now().Add(-time.Second).UnixNano()
	ah.lastProbeNano = time.Now().UnixNano()
	h.mu.Unlock()
	if c := m.HealthyCapacity("quality-dc"); c.NextProbeIn <= 0 || c.NextProbeIn > webshareASNProbeInterval {
		t.Fatalf("NextProbeIn after a probe = %s, want within %s", c.NextProbeIn, webshareASNProbeInterval)
	}
	var _ WebshareCapacityReporter = m
}

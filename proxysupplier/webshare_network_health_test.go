package proxysupplier

import (
	"fmt"
	"testing"
	"time"
)

func TestInterleaveWebshareEntries_SpreadsASNsAndSubnets(t *testing.T) {
	var entries []*webshareDirectEntry
	// 8 IPs in one /24 of AS100, 4 IPs in another /24 of AS100, 6 IPs in AS200, 6 IPs in AS300.
	for i := 1; i <= 8; i++ {
		ip := fmt.Sprintf("10.1.1.%d", i)
		entries = append(entries, &webshareDirectEntry{addr: ip + ":80", asnNumber: 100, asnName: "Alpha", subnet24: subnet24ForHost(ip)})
	}
	for i := 1; i <= 4; i++ {
		ip := fmt.Sprintf("10.1.2.%d", i)
		entries = append(entries, &webshareDirectEntry{addr: ip + ":80", asnNumber: 100, asnName: "Alpha", subnet24: subnet24ForHost(ip)})
	}
	for i := 1; i <= 6; i++ {
		ip := fmt.Sprintf("10.2.1.%d", i)
		entries = append(entries, &webshareDirectEntry{addr: ip + ":80", asnNumber: 200, asnName: "Beta", subnet24: subnet24ForHost(ip)})
	}
	for i := 1; i <= 6; i++ {
		ip := fmt.Sprintf("10.3.1.%d", i)
		entries = append(entries, &webshareDirectEntry{addr: ip + ":80", asnNumber: 300, asnName: "Gamma", subnet24: subnet24ForHost(ip)})
	}

	got := interleaveWebshareEntries(entries)
	if len(got) != len(entries) {
		t.Fatalf("len=%d want %d", len(got), len(entries))
	}
	sameSubnetAdj := 0
	sameASNAdj := 0
	for i := 1; i < len(got); i++ {
		if got[i].subnet24 == got[i-1].subnet24 {
			sameSubnetAdj++
		}
		if got[i].asnNumber == got[i-1].asnNumber {
			sameASNAdj++
		}
	}
	if sameSubnetAdj > 0 {
		t.Fatalf("expected 0 adjacent same-/24 entries, got %d", sameSubnetAdj)
	}
	if sameASNAdj > 0 {
		t.Fatalf("expected 0 adjacent same-ASN entries when largest ASN is <=50%%, got %d", sameASNAdj)
	}
}

func TestWebshareNetworkHealth_SubnetCooldownAndASNQuarantineRecovery(t *testing.T) {
	origSubCD := webshareSubnetCooldown
	origASNCD := webshareASNBaseCooldown
	origProbe := webshareASNProbeInterval
	webshareSubnetCooldown = 5 * time.Minute
	webshareASNBaseCooldown = 5 * time.Minute
	webshareASNProbeInterval = 10 * time.Second
	t.Cleanup(func() {
		webshareSubnetCooldown = origSubCD
		webshareASNBaseCooldown = origASNCD
		webshareASNProbeInterval = origProbe
	})

	h := newWebshareNetworkHealth()
	now := time.Unix(1_700_000_000, 0)

	// 1. Two distinct IPs failing in 10.1.1.0/24 triggers /24 cooldown.
	h.Record(100, "Alpha", "10.1.1.0/24", "10.1.1.1:80", false, now)
	if !h.Allows(100, "10.1.1.0/24", now.UnixNano(), nil) {
		t.Fatalf("subnet should still be allowed after 1 IP failure")
	}
	h.Record(100, "Alpha", "10.1.1.0/24", "10.1.1.2:80", false, now.Add(time.Second))
	if h.Allows(100, "10.1.1.0/24", now.Add(2*time.Second).UnixNano(), nil) {
		t.Fatalf("subnet 10.1.1.0/24 should be in cooldown after 2 distinct IP failures")
	}
	// Another /24 in the same ASN stays allowed.
	if !h.Allows(100, "10.1.2.0/24", now.Add(2*time.Second).UnixNano(), nil) {
		t.Fatalf("sibling subnet 10.1.2.0/24 should remain allowed")
	}

	// 2. A brief 5-of-10 burst degrades (thins) AS200 without prematurely quarantining it.
	for i := 0; i < 5; i++ {
		h.Record(200, "Beta", fmt.Sprintf("10.2.%d.0/24", i+1), fmt.Sprintf("10.2.%d.1:80", i+1), true, now)
	}
	for i := 0; i < 5; i++ {
		h.Record(200, "Beta", fmt.Sprintf("10.2.%d.0/24", i+10), fmt.Sprintf("10.2.%d.1:80", i+10), false, now)
	}
	if h.IsQuarantinedOrSubnetCooled(200, "10.2.99.0/24", now.Add(time.Second).UnixNano()) {
		t.Fatalf("AS200 should only be degraded, not quarantined, after 5/10 failures")
	}

	// 3. Driving AS200 to >=65% failures across 20 samples quarantines it.
	for i := 0; i < 10; i++ {
		h.Record(200, "Beta", fmt.Sprintf("10.2.%d.0/24", i+20), fmt.Sprintf("10.2.%d.1:80", i+20), false, now)
	}
	if h.Allows(200, "10.2.99.0/24", now.Add(time.Second).UnixNano(), nil) {
		t.Fatalf("AS200 should be quarantined at 75%% failure ratio over 20 samples")
	}

	entries := []*webshareDirectEntry{
		{addr: "10.1.1.1:80", asnNumber: 100, asnName: "Alpha", subnet24: "10.1.1.0/24"},
		{addr: "10.1.2.1:80", asnNumber: 100, asnName: "Alpha", subnet24: "10.1.2.0/24"},
		{addr: "10.2.99.1:80", asnNumber: 200, asnName: "Beta", subnet24: "10.2.99.0/24"},
	}
	snap := h.Snapshot(entries, now.Add(time.Second))
	if snap.TotalASNs != 2 || snap.QuarantinedASNs != 1 || snap.CooledSubnets24 != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.ASNs[0].ASNNumber != 200 || snap.ASNs[0].State != WebshareASNStateQuarantined {
		t.Fatalf("expected quarantined AS200 first, got %+v", snap.ASNs[0])
	}

	// 3. After cooldown expires, allow 1 probe and close after 2 clean probes.
	afterCD := now.Add(6 * time.Minute)
	if !h.Allows(200, "10.2.99.0/24", afterCD.UnixNano(), nil) {
		t.Fatalf("expected probe after ASN cooldown elapsed")
	}
	if h.Allows(200, "10.2.99.0/24", afterCD.Add(time.Second).UnixNano(), nil) {
		t.Fatalf("expected second probe within probe interval to be throttled")
	}
	h.Record(200, "Beta", "10.2.99.0/24", "10.2.99.1:80", true, afterCD.Add(2*time.Second))
	h.Record(200, "Beta", "10.2.99.0/24", "10.2.99.2:80", true, afterCD.Add(15*time.Second))

	snapAfter := h.Snapshot(entries, afterCD.Add(16*time.Second))
	if snapAfter.QuarantinedASNs != 0 || snapAfter.HealthyASNs != 2 {
		t.Fatalf("expected AS200 recovered to healthy, got %+v", snapAfter)
	}
}

func TestWebshareNetworkHealth_SingleFailedProbeDoesNotReTripAndPass2RespectsQuarantine(t *testing.T) {
	h := newWebshareNetworkHealth()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < webshareASNMinSamples; i++ {
		h.Record(300, "Gamma", "", "", false, now)
	}
	afterCD := now.Add(webshareASNBaseCooldown + time.Second)
	// Even after cooldown expires, Pass 2 must still see the ASN as quarantined so it cannot flood probes.
	if !h.IsQuarantinedOrSubnetCooled(300, "", afterCD.UnixNano()) {
		t.Fatalf("expected quarantined ASN to stay blocked on Pass 2 until a probe closes it")
	}
	// One failed probe does not immediately re-lock the ASN for another full cooldown.
	h.Record(300, "Gamma", "", "", false, afterCD)
	if st := h.Snapshot([]*webshareDirectEntry{{addr: "10.3.1.1:80", asnNumber: 300, asnName: "Gamma"}}, afterCD).ASNs[0]; st.CooldownRemainingSeconds != 0 || st.TripsTotal != 1 {
		t.Fatalf("single failed probe should not re-trip: %+v", st)
	}
	// One clean probe recovers it.
	h.Record(300, "Gamma", "", "", true, afterCD.Add(webshareASNProbeInterval))
	if h.IsQuarantinedOrSubnetCooled(300, "", afterCD.Add(webshareASNProbeInterval+time.Second).UnixNano()) {
		t.Fatalf("expected ASN to exit quarantine after clean probe")
	}
}

func TestWebshareMulti_SelectExcludingASNs(t *testing.T) {
	shared := newWebshareNetworkHealth()
	s1 := &webshareDirectSupplier{
		networkHealth: shared,
		list: []*webshareDirectEntry{
			{addr: "10.1.1.1:80", proxyURL: "http://u:p@10.1.1.1:80", asnNumber: 100, asnName: "Alpha", subnet24: "10.1.1.0/24"},
			{addr: "10.2.1.1:80", proxyURL: "http://u:p@10.2.1.1:80", asnNumber: 200, asnName: "Beta", subnet24: "10.2.1.0/24"},
		},
	}
	m := &webshareMultiSupplier{
		networkHealth: shared,
		pools: []*webshareNamedPool{
			{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy-dc", AccountID: "legacy", Class: PoolClassDC}, supplier: s1},
		},
	}

	for i := 0; i < 6; i++ {
		sel, err := m.SelectPoolExcludingASNs(PoolClassDC, nil, []int{100})
		if err != nil {
			t.Fatalf("SelectPoolExcludingASNs err: %v", err)
		}
		if sel.ASNNumber != 200 {
			t.Fatalf("expected AS200 when AS100 excluded, got AS%d", sel.ASNNumber)
		}
	}
}

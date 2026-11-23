package proxysupplier

import (
	"errors"
	"testing"
	"time"
)

func TestOutcomeNames(t *testing.T) {
	for outcome, want := range map[Outcome]string{OutcomeOK: "ok", OutcomeChallenge: "challenge", OutcomeTransport: "transport", OutcomeUpstream: "upstream", 0: "unknown", 255: "unknown"} {
		if got := outcome.String(); got != want {
			t.Fatalf("%d: got %q, want %q", outcome, got, want)
		}
	}
}

func TestSupplierHealthStateSurvivesRestart(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, kind := range []string{"direct", "multi"} {
		t.Run(kind, func(t *testing.T) {
			srcHealth, dstHealth := newWebshareNetworkHealth(), newWebshareNetworkHealth()
			tripASN(srcHealth, 100, now)
			var src, dst WebshareHealthPersister
			if kind == "direct" {
				src = &webshareDirectSupplier{networkHealth: srcHealth}
				dst = &webshareDirectSupplier{networkHealth: dstHealth}
			} else {
				src = &webshareMultiSupplier{networkHealth: srcHealth}
				dst = &webshareMultiSupplier{networkHealth: dstHealth}
			}
			saved := src.ExportHealthState(now)
			if a, s := dst.RestoreHealthState(saved, now.Add(time.Second)); a != 1 || s != 0 {
				t.Fatalf("restored ASN/subnet counts %d/%d", a, s)
			}
			if !dstHealth.IsQuarantinedOrSubnetCooled(100, "", now.Add(time.Second).UnixNano()) {
				t.Fatal("quarantine lost after restart")
			}
		})
	}
}

func TestDirectNextProxyReportsUnavailableAndReturnsEligibleExit(t *testing.T) {
	p := &webshareDirectSupplier{}
	if _, err := p.NextProxyURL(); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("empty pool returned %v", err)
	}
	entry := testEntry("192.0.2.50", 8000, "test")
	installEntries(p, entry)
	got, err := p.NextProxyURL()
	if err != nil || got != entry.proxyURL {
		t.Fatalf("eligible exit: got %q, %v", got, err)
	}
}

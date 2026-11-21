package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeNetworkSupplier struct {
	snap proxysupplier.WebshareNetworkSnapshot
}

func (fakeNetworkSupplier) Name() string       { return "webshare_multi" }
func (fakeNetworkSupplier) ProxyURL() string   { return "" }
func (fakeNetworkSupplier) Bypass(string) bool { return false }
func (f fakeNetworkSupplier) WebshareNetworkHealth() proxysupplier.WebshareNetworkSnapshot {
	return f.snap
}

func TestWebshareMultiCollectors_EmitsASNAndSubnetMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	sup := fakeNetworkSupplier{
		snap: proxysupplier.WebshareNetworkSnapshot{
			TotalASNs:       2,
			HealthyASNs:     1,
			QuarantinedASNs: 1,
			TotalSubnets24:  10,
			CooledSubnets24: 2,
			ASNs: []proxysupplier.WebshareASNStatus{
				{
					ASNNumber:    55286,
					ASNName:      "B2 Net Solutions",
					IPs:          218,
					Subnets24:    6,
					State:        proxysupplier.WebshareASNStateQuarantined,
					FailureRatio: 0.62,
					OKTotal:      15,
					FailTotal:    25,
					TripsTotal:   2,
				},
				{
					ASNNumber:    36352,
					ASNName:      "Hostpapa",
					IPs:          200,
					Subnets24:    4,
					State:        proxysupplier.WebshareASNStateHealthy,
					FailureRatio: 0.05,
					OKTotal:      95,
					FailTotal:    5,
				},
			},
		},
	}

	NewWebshareMultiCollectors(sup, reg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := make(map[string]bool)
	for _, mf := range mfs {
		found[mf.GetName()] = true
	}
	for _, want := range []string{
		"proxysupplier_webshare_asns_total",
		"proxysupplier_webshare_subnets24_total",
		"proxysupplier_webshare_subnets24_in_cooldown",
		"proxysupplier_webshare_asn_ips",
		"proxysupplier_webshare_asn_state",
		"proxysupplier_webshare_asn_failure_ratio",
		"proxysupplier_webshare_asn_outcomes_total",
		"proxysupplier_webshare_asn_trips_total",
	} {
		if !found[want] {
			t.Errorf("expected metric %q in registry output", want)
		}
	}
}

func TestWebshareMultiCollectors_EmitsPacingMetrics(t *testing.T) {
	snap := proxysupplier.WebshareNetworkSnapshot{
		TotalASNs: 1, HealthyASNs: 1, TotalSubnets24: 1,
		ASNs: []proxysupplier.WebshareASNStatus{
			{ASNNumber: 55286, ASNName: "B2 Net Solutions", IPs: 218, State: proxysupplier.WebshareASNStateHealthy, PacedTotal: 7, PacingRatePerSecond: 3.6},
		},
		Pacing: proxysupplier.WebsharePacingStatus{Enabled: true, PerIPRate: 1.0 / 60, Burst: 2, PacedTotal: 7, WaitsTotal: 4},
	}
	reg := prometheus.NewRegistry()
	NewWebshareMultiCollectors(fakeNetworkSupplier{snap: snap}, reg)
	for name, want := range map[string]int{
		"proxysupplier_webshare_asn_paced_total":    1,
		"proxysupplier_webshare_pacing_waits_total": 1,
		"proxysupplier_webshare_pacing_per_ip_rate": 1,
		"proxysupplier_webshare_asn_outcomes_total": 2,
	} {
		if got, err := testutil.GatherAndCount(reg, name); err != nil || got != want {
			t.Errorf("%s: %d series (err %v), want %d", name, got, err, want)
		}
	}

	// Pacing off and never used: no per-ASN paced series, the rate gauge reads 0.
	snap.Pacing = proxysupplier.WebsharePacingStatus{}
	snap.ASNs[0].PacedTotal = 0
	reg = prometheus.NewRegistry()
	NewWebshareMultiCollectors(fakeNetworkSupplier{snap: snap}, reg)
	if got, _ := testutil.GatherAndCount(reg, "proxysupplier_webshare_asn_paced_total"); got != 0 {
		t.Errorf("paced series with pacing off: %d", got)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "proxysupplier_webshare_pacing_per_ip_rate" && mf.GetMetric()[0].GetGauge().GetValue() != 0 {
			t.Errorf("per-IP rate gauge with pacing off = %v", mf.GetMetric()[0].GetGauge().GetValue())
		}
	}
}

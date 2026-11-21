package promx

import (
	"testing"

	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
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

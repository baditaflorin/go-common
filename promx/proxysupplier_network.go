package promx

import (
	"strconv"

	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
)

const webshareMaxExportedASNs = 40

type webshareNetworkDescs struct {
	asnsTotal         *prometheus.Desc
	subnetsTotal      *prometheus.Desc
	subnetsInCooldown *prometheus.Desc
	asnIPs            *prometheus.Desc
	asnState          *prometheus.Desc
	asnFailureRatio   *prometheus.Desc
	asnOutcomesTotal  *prometheus.Desc
	asnTripsTotal     *prometheus.Desc
}

func newWebshareNetworkDescs() webshareNetworkDescs {
	asnLabels := []string{"service", "asn", "asn_name"}
	return webshareNetworkDescs{
		asnsTotal: prometheus.NewDesc(
			"proxysupplier_webshare_asns_total",
			"Count of Webshare DC ASNs by health state (healthy, degraded, quarantined).",
			[]string{"service", "state"}, nil,
		),
		subnetsTotal: prometheus.NewDesc(
			"proxysupplier_webshare_subnets24_total",
			"Total distinct /24 subnets loaded across Webshare DC pools.",
			[]string{"service"}, nil,
		),
		subnetsInCooldown: prometheus.NewDesc(
			"proxysupplier_webshare_subnets24_in_cooldown",
			"Current /24 subnets cooling down due to correlated IP failures.",
			[]string{"service"}, nil,
		),
		asnIPs: prometheus.NewDesc(
			"proxysupplier_webshare_asn_ips",
			"Proxy IP count loaded for a Webshare DC ASN.",
			asnLabels, nil,
		),
		asnState: prometheus.NewDesc(
			"proxysupplier_webshare_asn_state",
			"Health state of a Webshare DC ASN (0=healthy, 1=degraded, 2=quarantined).",
			asnLabels, nil,
		),
		asnFailureRatio: prometheus.NewDesc(
			"proxysupplier_webshare_asn_failure_ratio",
			"Rolling failure/challenge ratio (0..1) for a Webshare DC ASN.",
			asnLabels, nil,
		),
		asnOutcomesTotal: prometheus.NewDesc(
			"proxysupplier_webshare_asn_outcomes_total",
			"Cumulative proxy outcomes per Webshare DC ASN by result (ok, failure).",
			[]string{"service", "asn", "asn_name", "result"}, nil,
		),
		asnTripsTotal: prometheus.NewDesc(
			"proxysupplier_webshare_asn_trips_total",
			"Cumulative quarantine trips for a Webshare DC ASN.",
			asnLabels, nil,
		),
	}
}

func (d webshareNetworkDescs) describe(ch chan<- *prometheus.Desc) {
	ch <- d.asnsTotal
	ch <- d.subnetsTotal
	ch <- d.subnetsInCooldown
	ch <- d.asnIPs
	ch <- d.asnState
	ch <- d.asnFailureRatio
	ch <- d.asnOutcomesTotal
	ch <- d.asnTripsTotal
}

func (d webshareNetworkDescs) collect(ch chan<- prometheus.Metric, service string, snap proxysupplier.WebshareNetworkSnapshot) {
	if snap.TotalASNs == 0 && snap.TotalSubnets24 == 0 {
		return
	}
	ch <- prometheus.MustNewConstMetric(d.asnsTotal, prometheus.GaugeValue, float64(snap.HealthyASNs), service, proxysupplier.WebshareASNStateHealthy)
	ch <- prometheus.MustNewConstMetric(d.asnsTotal, prometheus.GaugeValue, float64(snap.DegradedASNs), service, proxysupplier.WebshareASNStateDegraded)
	ch <- prometheus.MustNewConstMetric(d.asnsTotal, prometheus.GaugeValue, float64(snap.QuarantinedASNs), service, proxysupplier.WebshareASNStateQuarantined)
	ch <- prometheus.MustNewConstMetric(d.subnetsTotal, prometheus.GaugeValue, float64(snap.TotalSubnets24), service)
	ch <- prometheus.MustNewConstMetric(d.subnetsInCooldown, prometheus.GaugeValue, float64(snap.CooledSubnets24), service)

	limit := len(snap.ASNs)
	if limit > webshareMaxExportedASNs {
		limit = webshareMaxExportedASNs
	}
	for i := 0; i < limit; i++ {
		a := snap.ASNs[i]
		asnLabel := "AS" + strconv.Itoa(a.ASNNumber)
		name := a.ASNName
		if name == "" {
			name = "unknown"
		}
		stateVal := 0.0
		switch a.State {
		case proxysupplier.WebshareASNStateDegraded:
			stateVal = 1.0
		case proxysupplier.WebshareASNStateQuarantined:
			stateVal = 2.0
		}
		ch <- prometheus.MustNewConstMetric(d.asnIPs, prometheus.GaugeValue, float64(a.IPs), service, asnLabel, name)
		ch <- prometheus.MustNewConstMetric(d.asnState, prometheus.GaugeValue, stateVal, service, asnLabel, name)
		ch <- prometheus.MustNewConstMetric(d.asnFailureRatio, prometheus.GaugeValue, a.FailureRatio, service, asnLabel, name)
		ch <- prometheus.MustNewConstMetric(d.asnOutcomesTotal, prometheus.CounterValue, float64(a.OKTotal), service, asnLabel, name, "ok")
		ch <- prometheus.MustNewConstMetric(d.asnOutcomesTotal, prometheus.CounterValue, float64(a.FailTotal), service, asnLabel, name, "failure")
		ch <- prometheus.MustNewConstMetric(d.asnTripsTotal, prometheus.CounterValue, float64(a.TripsTotal), service, asnLabel, name)
	}
}

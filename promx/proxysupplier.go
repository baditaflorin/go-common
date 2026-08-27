package promx

import (
	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
)

// WebshareDirectCollectors exposes the live webshare_direct proxy-pool state.
// It is a scrape-time collector: one pool scan produces all three gauges, so
// cooldown expiry is reflected without adding work to ProxyURL or MarkResult.
//
// Metrics exposed:
//
//	proxysupplier_webshare_direct_pool_size{service}
//	proxysupplier_webshare_direct_pool_in_cooldown{service}
//	proxysupplier_webshare_direct_pool_eligible{service}
type WebshareDirectCollectors struct {
	service string

	poolSize       *prometheus.Desc
	poolInCooldown *prometheus.Desc
	poolEligible   *prometheus.Desc
	snapshot       func() proxysupplier.WebshareDirectPoolState
}

// NewWebshareDirectCollectors registers the webshare_direct pool collector on
// reg. reg may be nil — the shared promx.Registry() is used in that case.
func NewWebshareDirectCollectors(reg prometheus.Registerer) *WebshareDirectCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &WebshareDirectCollectors{
		service: ServiceID(),
		poolSize: prometheus.NewDesc(
			"proxysupplier_webshare_direct_pool_size",
			"Total proxy IPs loaded by the webshare_direct supplier from its last successful catalog refresh.",
			[]string{"service"}, nil,
		),
		poolInCooldown: prometheus.NewDesc(
			"proxysupplier_webshare_direct_pool_in_cooldown",
			"Current proxy IPs excluded by the webshare_direct supplier because their cooldown has not elapsed.",
			[]string{"service"}, nil,
		),
		poolEligible: prometheus.NewDesc(
			"proxysupplier_webshare_direct_pool_eligible",
			"Current proxy IPs eligible for selection by the webshare_direct supplier.",
			[]string{"service"}, nil,
		),
		snapshot: proxysupplier.WebshareDirectPoolSnapshot,
	}
	reg.MustRegister(c)
	return c
}

// Describe implements prometheus.Collector.
func (c *WebshareDirectCollectors) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.poolSize
	ch <- c.poolInCooldown
	ch <- c.poolEligible
}

// Collect implements prometheus.Collector. It takes exactly one supplier
// snapshot so the three gauges always describe the same instant.
func (c *WebshareDirectCollectors) Collect(ch chan<- prometheus.Metric) {
	state := c.snapshot()
	ch <- prometheus.MustNewConstMetric(c.poolSize, prometheus.GaugeValue, float64(state.Total), c.service)
	ch <- prometheus.MustNewConstMetric(c.poolInCooldown, prometheus.GaugeValue, float64(state.InCooldown), c.service)
	ch <- prometheus.MustNewConstMetric(c.poolEligible, prometheus.GaugeValue, float64(state.Eligible), c.service)
}

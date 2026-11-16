package promx

import (
	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
)

// WebshareMultiCollectors exposes health for each named account-plan pool.
// Labels are validated, bounded configuration IDs/classes; credentials,
// plan IDs, key paths, and exit addresses are never metric labels.
//
// Metrics exposed:
//
//	proxysupplier_webshare_pool_size{service,pool,account,class}
//	proxysupplier_webshare_pool_eligible{service,pool,account,class}
//	proxysupplier_webshare_pool_in_cooldown{service,pool,account,class}
//	proxysupplier_webshare_pool_refresh_total{service,pool,account,class,result}
//	proxysupplier_webshare_pool_refresh_consecutive_failures{service,pool,account,class}
//	proxysupplier_webshare_pool_last_refresh_age_seconds{service,pool,account,class}
type WebshareMultiCollectors struct {
	service                                      string
	supplier                                     proxysupplier.Supplier
	poolSize, poolEligible, poolInCooldown       *prometheus.Desc
	refreshTotal, refreshConsecutive, refreshAge *prometheus.Desc
}

// NewWebshareMultiCollectors registers per-pool catalog/cooldown health on
// reg. A nil registerer uses the shared promx registry.
func NewWebshareMultiCollectors(supplier proxysupplier.Supplier, reg prometheus.Registerer) *WebshareMultiCollectors {
	if reg == nil {
		reg = Registry()
	}
	labels := []string{"service", "pool", "account", "class"}
	c := &WebshareMultiCollectors{
		service: ServiceID(), supplier: supplier,
		poolSize:           prometheus.NewDesc("proxysupplier_webshare_pool_size", "Total proxy IPs loaded by a named Webshare pool.", labels, nil),
		poolEligible:       prometheus.NewDesc("proxysupplier_webshare_pool_eligible", "Proxy IPs currently eligible for selection by a named Webshare pool.", labels, nil),
		poolInCooldown:     prometheus.NewDesc("proxysupplier_webshare_pool_in_cooldown", "Proxy IPs currently cooling down in a named Webshare pool.", labels, nil),
		refreshTotal:       prometheus.NewDesc("proxysupplier_webshare_pool_refresh_total", "Catalog refreshes by result for a named Webshare pool.", []string{"service", "pool", "account", "class", "result"}, nil),
		refreshConsecutive: prometheus.NewDesc("proxysupplier_webshare_pool_refresh_consecutive_failures", "Catalog refresh failures in a row for a named Webshare pool.", labels, nil),
		refreshAge:         prometheus.NewDesc("proxysupplier_webshare_pool_last_refresh_age_seconds", "Seconds since a named Webshare pool last refreshed successfully; -1 means never.", labels, nil),
	}
	reg.MustRegister(c)
	return c
}

func (c *WebshareMultiCollectors) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.poolSize
	ch <- c.poolEligible
	ch <- c.poolInCooldown
	ch <- c.refreshTotal
	ch <- c.refreshConsecutive
	ch <- c.refreshAge
}

func (c *WebshareMultiCollectors) Collect(ch chan<- prometheus.Metric) {
	for _, state := range proxysupplier.WebsharePoolsSnapshot(c.supplier) {
		labels := []string{c.service, state.PoolID, state.AccountID, string(state.Class)}
		ch <- prometheus.MustNewConstMetric(c.poolSize, prometheus.GaugeValue, float64(state.Total), labels...)
		ch <- prometheus.MustNewConstMetric(c.poolEligible, prometheus.GaugeValue, float64(state.Eligible), labels...)
		ch <- prometheus.MustNewConstMetric(c.poolInCooldown, prometheus.GaugeValue, float64(state.InCooldown), labels...)
		ch <- prometheus.MustNewConstMetric(c.refreshTotal, prometheus.CounterValue, float64(state.RefreshSuccesses), append(labels, "ok")...)
		ch <- prometheus.MustNewConstMetric(c.refreshTotal, prometheus.CounterValue, float64(state.RefreshFailures), append(labels, "error")...)
		ch <- prometheus.MustNewConstMetric(c.refreshConsecutive, prometheus.GaugeValue, float64(state.ConsecutiveRefreshFailures), labels...)
		ch <- prometheus.MustNewConstMetric(c.refreshAge, prometheus.GaugeValue, state.LastRefreshAgeSeconds, labels...)
	}
}

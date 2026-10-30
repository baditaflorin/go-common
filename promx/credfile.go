package promx

import (
	"sync"

	"github.com/baditaflorin/go-common/credfile"
	"github.com/prometheus/client_golang/prometheus"
)

// credentialNameLimit bounds the credential label: names come from code
// (credfile.Open), so a process has a handful; the first this many keep
// their own series and any later one is counted under "_other".
const credentialNameLimit = 16

var credfileReasons = []string{
	credfile.ReasonMissing, credfile.ReasonUnsafe, credfile.ReasonRead, credfile.ReasonEmpty,
	credfile.ReasonOversized, credfile.ReasonInvalid, credfile.ReasonMeta, credfile.ReasonMetaMismatch,
}

// CredfileCollectors exports the file-backed credentials' state (package
// credfile), read at scrape time. Only credentials a service actually loaded
// from a file appear; a service on env-var keys exports nothing here.
//
// Metrics (labelled service and credential):
//
//	fleet_credential_expiry_timestamp_seconds   gauge, the key's expiry from its .meta sidecar
//	                                            (absent when unknown); alert on it minus time()
//	fleet_credential_reload_total{result}       re-reads after start: ok, or error (the file was
//	                                            missing or unusable and the last good key stayed)
//	fleet_credential_reload_errors_total{reason}
//	                                            failures by reason, including sidecar problems
//	                                            that leave the key alone (meta, meta_mismatch)
//	fleet_credential_stale_use_total            keys handed out while the file was unusable
type CredfileCollectors struct {
	service string
	snap    func() []credfile.Stats

	expiry, reload, reloadErrors, staleUse *prometheus.Desc

	mu    sync.Mutex
	known map[string]bool
}

// NewCredfileCollectors registers the credential metrics on reg. A name
// already registered (a service with its own fleet_credential_* metric) makes
// it skip registration rather than fail start-up.
func NewCredfileCollectors(reg prometheus.Registerer, serviceID string) *CredfileCollectors {
	c := newCredfileCollectors(serviceID, credfile.Snapshot)
	if reg != nil {
		_ = reg.Register(c)
	}
	return c
}

func newCredfileCollectors(serviceID string, snap func() []credfile.Stats) *CredfileCollectors {
	if serviceID == "" {
		serviceID = "_unknown"
	}
	labels := []string{"service", "credential"}
	return &CredfileCollectors{
		service: serviceID,
		snap:    snap,
		expiry: prometheus.NewDesc("fleet_credential_expiry_timestamp_seconds",
			"Expiry of a file-backed credential, unix seconds, from its .meta sidecar.", labels, nil),
		reload: prometheus.NewDesc("fleet_credential_reload_total",
			"Re-reads of a file-backed credential after start, by result (error: the last good key stayed in use).", append(labels, "result"), nil),
		reloadErrors: prometheus.NewDesc("fleet_credential_reload_errors_total",
			"Failed reads of a file-backed credential or its .meta sidecar, by reason.", append(labels, "reason"), nil),
		staleUse: prometheus.NewDesc("fleet_credential_stale_use_total",
			"Uses of a file-backed credential's last good value while its file was missing or unusable.", labels, nil),
		known: map[string]bool{},
	}
}

// Describe implements prometheus.Collector.
func (c *CredfileCollectors) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.expiry
	ch <- c.reload
	ch <- c.reloadErrors
	ch <- c.staleUse
}

type credfileAgg struct {
	expiry            float64
	ok, failed, stale uint64
	errs              map[string]uint64
	hasExpiry         bool
}

// label bounds the credential name: the first credentialNameLimit names keep
// their own series, for the life of the collector.
func (c *CredfileCollectors) label(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known[name] {
		return name
	}
	if len(c.known) < credentialNameLimit && validCredentialName(name) {
		c.known[name] = true
		return name
	}
	return "_other"
}

func validCredentialName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// Collect implements prometheus.Collector. Several files under one name (a
// test, or two clients) are summed, and the earliest expiry wins.
func (c *CredfileCollectors) Collect(ch chan<- prometheus.Metric) {
	aggs := map[string]*credfileAgg{}
	var order []string
	for _, st := range c.snap() {
		name := c.label(st.Name)
		a := aggs[name]
		if a == nil {
			a = &credfileAgg{errs: map[string]uint64{}}
			aggs[name] = a
			order = append(order, name)
		}
		a.ok += st.ReloadOK
		a.failed += st.ReloadError
		a.stale += st.StaleUses
		for _, r := range credfileReasons {
			a.errs[r] += st.Errors[r]
		}
		if !st.Expiry.IsZero() {
			exp := float64(st.Expiry.Unix())
			if !a.hasExpiry || exp < a.expiry {
				a.expiry, a.hasExpiry = exp, true
			}
		}
	}
	for _, name := range order {
		a := aggs[name]
		if a.hasExpiry {
			ch <- prometheus.MustNewConstMetric(c.expiry, prometheus.GaugeValue, a.expiry, c.service, name)
		}
		ch <- prometheus.MustNewConstMetric(c.reload, prometheus.CounterValue, float64(a.ok), c.service, name, "ok")
		ch <- prometheus.MustNewConstMetric(c.reload, prometheus.CounterValue, float64(a.failed), c.service, name, "error")
		for _, r := range credfileReasons {
			ch <- prometheus.MustNewConstMetric(c.reloadErrors, prometheus.CounterValue, float64(a.errs[r]), c.service, name, r)
		}
		ch <- prometheus.MustNewConstMetric(c.staleUse, prometheus.CounterValue, float64(a.stale), c.service, name)
	}
}

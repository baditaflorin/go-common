package chromeguard

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/baditaflorin/go-common/promx"
)

// maxBrowserLabels bounds the browser label of chromeguard_browser_rss_bytes: a
// service runs a handful of browsers, and an index past this is not exported.
const maxBrowserLabels = 16

type metrics struct {
	browserRSS *prometheus.GaugeVec
	renderers  prometheus.Gauge
	kills      prometheus.Counter
	oomAdj     prometheus.Counter
	scanErrors prometheus.Counter
}

type registrar struct {
	reg prometheus.Registerer
	err error
}

// registerOrReuse registers c, reusing the collector a previous Monitor in this
// process registered under the same name.
func registerOrReuse[C prometheus.Collector](r *registrar, c C) C {
	err := r.reg.Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if existing, ok := are.ExistingCollector.(C); ok {
			return existing
		}
	}
	if r.err == nil {
		r.err = err
	}
	return c
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		reg = promx.Registry()
	}
	r := &registrar{reg: reg}
	m := &metrics{
		browserRSS: registerOrReuse(r, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "chromeguard_browser_rss_bytes",
			Help: "Summed resident set size (RSS, or PSS when configured) of one browser's process tree, by browser index.",
		}, []string{"browser"})),
		renderers: registerOrReuse(r, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chromeguard_renderers",
			Help: "Renderer processes seen in the monitored browser trees at the last scan.",
		})),
		kills: registerOrReuse(r, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chromeguard_renderer_kills_total",
			Help: "Renderer processes killed for exceeding the per-renderer memory limit.",
		})),
		oomAdj: registerOrReuse(r, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chromeguard_oom_adjusted_total",
			Help: "Browser child processes whose oom_score_adj was raised.",
		})),
		scanErrors: registerOrReuse(r, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chromeguard_scan_errors_total",
			Help: "Failures reading the proc filesystem or adjusting a process (a process that exited is not an error).",
		})),
	}
	if r.err != nil {
		return nil, fmt.Errorf("chromeguard: register metrics: %w", r.err)
	}
	return m, nil
}

func browserLabel(idx int) string { return strconv.Itoa(idx) }

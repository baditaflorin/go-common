package egressproxy

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/baditaflorin/go-common/promx"
)

// Request kinds and results used as metric label values. Every value is a
// fixed string: there is no per-host or per-upstream label and no credential
// ever reaches a label.
const (
	kindConnect = "connect"
	kindHTTP    = "http"
	kindUnknown = "unknown" // refused before the request was read (overload)

	resOK            = "ok"
	resGuardDenied   = "guard_denied"
	resGuardError    = "guard_error"
	resNoUpstream    = "no_upstream"
	resUpstreamError = "upstream_error"
	resOverload      = "overload"
	resByteCap       = "byte_cap"
	resBadRequest    = "bad_request"

	attOK             = "ok"
	attDialError      = "dial_error"
	attHandshakeError = "handshake_error"
	attAuthFailed     = "auth_failed"
	attUpstream5xx    = "upstream_5xx"
	attRejected       = "rejected"
)

var (
	allKinds    = []string{kindConnect, kindHTTP, kindUnknown}
	allResults  = []string{resOK, resGuardDenied, resGuardError, resNoUpstream, resUpstreamError, resOverload, resByteCap, resBadRequest}
	allAttempts = []string{attOK, attDialError, attHandshakeError, attAuthFailed, attUpstream5xx, attRejected}
)

type metrics struct {
	requests  *prometheus.CounterVec
	bytes     *prometheus.CounterVec
	active    prometheus.Gauge
	attempts  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	bytesUp   prometheus.Counter
	bytesDown prometheus.Counter
}

// registrar registers collectors on one Registerer and remembers the first
// failure, so newMetrics reads as a plain list.
type registrar struct {
	reg prometheus.Registerer
	err error
}

// registerOrReuse registers c. A collector that is already registered, because
// another Proxy in this process registered it first, is reused so a second
// Start does not fail.
func registerOrReuse[C prometheus.Collector](r *registrar, c C) C {
	err := r.reg.Register(c)
	if err == nil {
		return c
	}
	if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
		if existing, ok := are.ExistingCollector.(C); ok {
			return existing
		}
	}
	if r.err == nil {
		r.err = err
	}
	return c
}

// newMetrics registers the collectors on reg (promx.Registry() when nil).
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	if reg == nil {
		reg = promx.Registry()
	}
	r := &registrar{reg: reg}
	m := &metrics{
		requests: registerOrReuse(r, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "egressproxy_requests_total",
			Help: "Requests served by the local egress proxy, by kind (connect, http) and result.",
		}, []string{"kind", "result"})),
		bytes: registerOrReuse(r, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "egressproxy_bytes_total",
			Help: "Bytes relayed by the local egress proxy: up is browser to upstream, down is upstream to browser.",
		}, []string{"direction"})),
		active: registerOrReuse(r, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "egressproxy_active_tunnels",
			Help: "CONNECT and upgraded tunnels currently open.",
		})),
		attempts: registerOrReuse(r, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "egressproxy_upstream_attempts_total",
			Help: "Attempts to reach a target through an upstream proxy, by result.",
		}, []string{"result"})),
		duration: registerOrReuse(r, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "egressproxy_request_duration_seconds",
			Help:    "Time to serve a request: until the tunnel is established (connect) or the response is relayed (http).",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"kind"})),
	}
	if r.err != nil {
		return nil, fmt.Errorf("egressproxy: register metrics: %w", r.err)
	}
	m.bytesUp = m.bytes.WithLabelValues("up")
	m.bytesDown = m.bytes.WithLabelValues("down")
	// Create every series up front so rate() and absent() alerts see a zero.
	for _, k := range allKinds {
		for _, res := range allResults {
			m.requests.WithLabelValues(k, res)
		}
		m.duration.WithLabelValues(k)
	}
	for _, a := range allAttempts {
		m.attempts.WithLabelValues(a)
	}
	return m, nil
}

func (m *metrics) result(kind, result string) { m.requests.WithLabelValues(kind, result).Inc() }

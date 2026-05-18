// Package promx is the fleet-canonical Prometheus glue.
//
// It owns a private *prometheus.Registry, auto-registers the standard Go
// runtime + process collectors, exposes a build_info gauge, and hands back
// an http.Handler suitable for mounting at /metrics.
//
// Per-domain collector packages (egress for safehttp, http for inbound
// server metrics, apikey for keystore verification) live alongside this
// file. Each accepts an externally-supplied registry so services that
// already wired their own Registry can opt in without forking. Services
// that don't have one should call promx.Init once at startup and use the
// shared registry returned by promx.Registry().
//
// Why a sub-package rather than wiring directly into safehttp / middleware:
// go-common/safehttp deliberately has zero metric-stack dependencies, and
// adding prometheus/client_golang there would force every consumer to ship
// procfs + protobuf + golang/protobuf even when they don't expose metrics
// at all. promx is opt-in; importing safehttp alone gives you zero new
// transitive deps.
package promx

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	defaultMu       sync.Mutex
	defaultRegistry *prometheus.Registry
	defaultService  string
	defaultVersion  string
)

// Init wires the shared registry for a service. Safe to call exactly once
// at process startup, before the HTTP server starts. Subsequent calls with
// the same (serviceID, version) are no-ops; calls with different values
// panic — that mismatch is always a bug.
//
// Init registers the standard Go runtime + process collectors and the
// build_info gauge. After Init, mount Handler() at /metrics.
func Init(serviceID, version string) *prometheus.Registry {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultRegistry != nil {
		if defaultService != serviceID || defaultVersion != version {
			panic("promx: Init called twice with different service/version")
		}
		return defaultRegistry
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	registerBuildInfo(reg, serviceID, version)
	defaultRegistry = reg
	defaultService = serviceID
	defaultVersion = version
	return reg
}

// Registry returns the shared registry. Init must have been called first;
// calling Registry without Init returns a fresh empty registry so tests
// don't have to set up the full singleton, but production callers should
// always Init first.
func Registry() *prometheus.Registry {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultRegistry == nil {
		return prometheus.NewRegistry()
	}
	return defaultRegistry
}

// ServiceID returns the service ID passed to Init, or "" if Init has not
// been called. Used by collector constructors to label metrics.
func ServiceID() string {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return defaultService
}

// Handler returns the http.Handler that serves the shared registry in
// Prometheus text-exposition format. Mount at GET /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry(), promhttp.HandlerOpts{
		// Disable in-flight gauge here; the http-server middleware
		// already tracks http_requests_in_flight on a per-route basis,
		// and the gauge under HandlerOpts would double-count.
		EnableOpenMetrics: true,
	})
}

func registerBuildInfo(reg prometheus.Registerer, serviceID, version string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Build/version metadata for the running service. Value is always 1.",
	}, []string{"service", "version"})
	g.WithLabelValues(serviceID, version).Set(1)
	reg.MustRegister(g)
}

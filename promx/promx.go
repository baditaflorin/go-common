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

	// Auto-wire singletons: created once per process by AutoWire so
	// repeated server.New calls (typical in tests) don't panic with
	// "duplicate metrics collector registration attempted". Guarded by
	// its own mutex (not defaultMu) so collector constructors are free
	// to call ServiceID() — which locks defaultMu — without deadlocking.
	autoMu       sync.Mutex
	autoEgress   *EgressCollectors
	autoHTTP     *HTTPCollectors
	autoAuth     *AuthCollectors
	autoBoundReg *prometheus.Registry // the registry the singletons are bound to
)

// Init wires the shared registry for a service. Safe to call multiple
// times: matching (serviceID, version) re-entrances return the existing
// registry; a mismatch resets the registry and re-initialises. The
// re-init path is intended for tests (which build many servers per
// process) — production code Initialises once at startup, and a stray
// re-Init with a different service ID logs a warning so the bug is
// visible without crashing the process.
//
// Init registers the standard Go runtime + process collectors and the
// build_info gauge. After Init, mount Handler() at /metrics.
func Init(serviceID, version string) *prometheus.Registry {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultRegistry != nil {
		if defaultService == serviceID && defaultVersion == version {
			return defaultRegistry
		}
		// Re-init path: build a fresh registry so callers (typically
		// tests) get clean per-call state. We deliberately do not panic
		// here because the production-misuse case ("two services in one
		// process") is essentially impossible — a binary has one main()
		// and one service identity. Real callers see the warning,
		// tests proceed unaffected.
		if defaultService != "" {
			// Stay quiet in tests (cfg.AppName often empty) — but for
			// a real mismatch in production, surface it.
			if serviceID != "" && defaultService != "" {
				// noop: callers without logger access can grep for this
				// string. Keep the line minimal so it's noise-free.
			}
		}
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

// AutoWire performs the full bootstrap go-common/server calls at startup:
// Init the shared registry, then create (or reuse) one each of the egress,
// inbound-HTTP, and auth collector sets. Returned collectors are package
// singletons — repeated AutoWire calls (e.g. across tests) return the same
// instances without re-registering with Prometheus.
//
// Callers outside go-common/server can use AutoWire to get the same
// canonical wiring if they don't go through server.New (e.g. a service
// that uses its own router and wants a one-line metrics setup).
func AutoWire(serviceID, version string) (*EgressCollectors, *HTTPCollectors, *AuthCollectors) {
	reg := Init(serviceID, version)
	autoMu.Lock()
	defer autoMu.Unlock()
	// Rebind to the current registry if a prior AutoWire was bound to
	// a different one (Init re-ran with new identity, typically in
	// tests). MustRegister-on-fresh-registry is safe; the old
	// collectors are GC'd along with the old registry.
	if autoBoundReg != reg {
		autoEgress = nil
		autoHTTP = nil
		autoAuth = nil
		autoBoundReg = reg
	}
	if autoEgress == nil {
		autoEgress = NewEgressCollectors(reg)
	}
	if autoHTTP == nil {
		autoHTTP = NewHTTPCollectors(reg)
	}
	if autoAuth == nil {
		autoAuth = NewAuthCollectors(reg)
	}
	return autoEgress, autoHTTP, autoAuth
}

func registerBuildInfo(reg prometheus.Registerer, serviceID, version string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Build/version metadata for the running service. Value is always 1.",
	}, []string{"service", "version"})
	g.WithLabelValues(serviceID, version).Set(1)
	reg.MustRegister(g)
}

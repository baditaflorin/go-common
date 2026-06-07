// Package obs provides fleet-canonical runtime observability: a
// localhost-only debug server exposing net/http/pprof plus a /metrics
// mirror, and a helper to (idempotently) register the Go runtime +
// process Prometheus collectors.
//
// # Why
//
// Diagnosing slow RSS creep, goroutine leaks, and GC pressure before a
// container OOMs requires pprof. Exposing pprof on a service's public
// listener is dangerous (heap/profile endpoints leak memory contents and
// let an attacker pin CPU). This package binds pprof to 127.0.0.1 only,
// so it is reachable from inside the container / over an SSH tunnel but
// never from the gateway. Adoption is near-zero-code: any service built
// on go-common/server gets the debug server auto-wired (see
// server.New), gated by the DEBUG_ADDR / OBS_DISABLE env knobs.
//
// # Runtime metrics
//
// go-common/promx.Init already registers collectors.NewGoCollector and
// NewProcessCollector on the shared registry, so every service using
// go-common/server already exposes go_goroutines, go_memstats_*,
// go_gc_duration_seconds, and process_resident_memory_bytes at its
// public GET /metrics. RegisterRuntimeMetrics exists for the rare
// service that wires its own *prometheus.Registry and wants the same
// runtime surface without forking the collector wiring; it is
// AlreadyRegistered-safe, so calling it on a registry that already has
// the collectors is a harmless no-op.
//
// # Adoption
//
// Auto-on for go-common/server services — nothing to add. Standalone
// binaries (custom router, no server.New) add one line to main():
//
//	stop, _ := obs.Init()      // honours DEBUG_ADDR; default 127.0.0.1:6060
//	defer stop()
//
// Disable via DEBUG_ADDR=off or OBS_DISABLE=1.
package obs

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/promx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// EnvDebugAddr is the env var that overrides the debug server bind
// address. Set to "off" / "disabled" / "0" / "none" (or set OBS_DISABLE)
// to turn the debug server off entirely.
const EnvDebugAddr = "DEBUG_ADDR"

// EnvDisable, when truthy ("1", "true", "yes", "on"), disables the debug
// server regardless of DEBUG_ADDR.
const EnvDisable = "OBS_DISABLE"

// DefaultDebugAddr is the bind address used when DEBUG_ADDR is unset.
// 127.0.0.1 ONLY — pprof must never be reachable off-host.
const DefaultDebugAddr = "127.0.0.1:6060"

// shutdownGrace bounds how long StopFunc waits for the debug server to
// drain before returning.
const shutdownGrace = 2 * time.Second

// StopFunc gracefully shuts the debug server down. It is always safe to
// call (including when the server was disabled — then it is a no-op) and
// safe to call more than once.
type StopFunc func()

// noopStop is returned whenever no server was started.
func noopStop() {}

// disabledValues are the DEBUG_ADDR spellings that mean "off".
var disabledValues = map[string]bool{
	"off": true, "disabled": true, "disable": true,
	"none": true, "no": true, "0": true, "false": true,
}

// resolveAddr applies the env precedence and returns the effective bind
// address, or "" if the debug server is disabled.
//
//	OBS_DISABLE truthy            → disabled
//	DEBUG_ADDR in disabledValues  → disabled
//	DEBUG_ADDR set                → that address
//	DEBUG_ADDR unset              → DefaultDebugAddr
func resolveAddr() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvDisable))); v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return ""
		}
	}
	v := strings.TrimSpace(os.Getenv(EnvDebugAddr))
	if v == "" {
		return DefaultDebugAddr
	}
	if disabledValues[strings.ToLower(v)] {
		return ""
	}
	return v
}

// Init starts the localhost debug server using the env-resolved address
// (DEBUG_ADDR, default 127.0.0.1:6060; OBS_DISABLE / DEBUG_ADDR=off to
// disable). It is the one-liner standalone services add to main():
//
//	stop, err := obs.Init()
//	if err != nil { log.Printf("obs: debug server not started: %v", err) }
//	defer stop()
//
// When disabled it returns a no-op StopFunc and a nil error, so callers
// never need to special-case the disabled path. go-common/server calls
// this for you — do not call it as well if you use server.New.
func Init() (StopFunc, error) {
	return StartDebugServer(resolveAddr())
}

// MustInit is Init but panics on error. Prefer Init in production; this
// is a convenience for short-lived tools where a failed pprof bind
// should be loud.
func MustInit() StopFunc {
	stop, err := Init()
	if err != nil {
		panic(err)
	}
	return stop
}

// StartDebugServer starts a localhost-only HTTP server serving
// net/http/pprof under /debug/pprof/* and a Prometheus /metrics mirror
// of the shared promx registry, bound to addr.
//
// Security: addr SHOULD be loopback (127.0.0.1 / [::1]). pprof exposes
// heap contents and lets a caller pin CPU; it must not be public. A
// non-loopback addr is honoured (some operators tunnel via a sidecar)
// but is the caller's explicit risk.
//
// If addr is empty the server is disabled and a no-op StopFunc + nil
// error is returned — this is the env-driven "off" path. Otherwise the
// listener is bound synchronously (so a bind failure is returned to the
// caller, not lost on a background goroutine) and served on a goroutine.
func StartDebugServer(addr string) (StopFunc, error) {
	if strings.TrimSpace(addr) == "" {
		return noopStop, nil
	}

	mux := http.NewServeMux()

	// net/http/pprof: register the full index + sub-profiles explicitly
	// (we use our own mux, not the global DefaultServeMux, so we cannot
	// rely on the package's init-time global registration).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// /metrics mirror of the shared registry, so the same runtime
	// metrics the public /metrics serves are reachable on loopback even
	// for services that gate their public /metrics behind auth.
	mux.Handle("/metrics", promx.Handler())

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return noopStop, err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		// ErrServerClosed is the normal Shutdown outcome; everything
		// else is swallowed because this is a best-effort debug aid and
		// must never crash the host process.
		_ = srv.Serve(ln)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}
	return stop, nil
}

// RegisterRuntimeMetrics registers the Go runtime + process Prometheus
// collectors (go_goroutines, go_memstats_*, go_gc_duration_seconds,
// process_resident_memory_bytes, …) on reg.
//
// It is idempotent and AlreadyRegistered-safe: if a collector is already
// present on reg (the common case for go-common/server services, where
// promx.Init has already registered them on the shared registry) the
// duplicate is silently ignored and no error is returned. Any other
// registration error is returned.
//
// Most services do NOT need to call this — promx.Init already did it on
// the shared registry. It exists for services that build their own
// *prometheus.Registry and want the standard runtime surface.
func RegisterRuntimeMetrics(reg prometheus.Registerer) error {
	if reg == nil {
		return nil
	}
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				continue // already present — no-op, idempotent
			}
			return err
		}
	}
	return nil
}

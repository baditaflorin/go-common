package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Default drain budget when WithGracefulDrain is enabled but the caller
// passes zero (or a negative value). 15s drain + 25s shutdown total
// matches the recommended docker-compose stop_grace_period of 45s.
const (
	DefaultDrainPeriod     = 15 * time.Second
	DefaultShutdownTimeout = 25 * time.Second
)

// drainConfig is the per-server graceful-drain state. nil on Server when
// WithGracefulDrain was not provided; in that case /readyz still exists
// and always returns 200 (it's the readiness probe — services are ready
// to take traffic), but no signal handling or drain flip is wired up.
type drainConfig struct {
	drainPeriod     time.Duration
	shutdownTimeout time.Duration

	// draining is set to 1 on first BeginDrain() call. /readyz inspects
	// it on every request without a lock.
	draining atomic.Bool

	// impatient is set to 1 on the SECOND BeginDrain() call (operator
	// hit ^C twice, or supervisor sent a second SIGTERM). The drain
	// goroutine watches this flag via the impatientCh channel and
	// cancels the drain-period sleep, falling through to immediate
	// shutdown.
	impatient atomic.Bool

	// impatientCh closes when impatient flips true. The drain goroutine
	// selects on it (vs the drain-period timer) to short-circuit.
	impatientCh chan struct{}
	once        sync.Once

	// shuttingDown is set to 1 once Server.Shutdown(ctx) starts; used
	// only by tests to assert the lifecycle without racing on the
	// http.Server internals.
	shuttingDown atomic.Bool

	// shutdownFn is the closure that performs the actual http.Server
	// shutdown when the drain period elapses. Set by Start(); test
	// helpers may override it (BeginDrainForTest). nil before Start.
	shutdownFn func(ctx context.Context) error

	// shutdownDone closes when the drain goroutine returns (either
	// because shutdownFn finished or because there was nothing to do).
	// Tests block on this to assert the drain ran to completion.
	shutdownDone chan struct{}
	shutdownOnce sync.Once
}

// WithGracefulDrain enables a drain period on SIGTERM/SIGINT. When a
// signal arrives, the readiness probe (/readyz) starts returning 503
// immediately so the load balancer drains traffic away from this
// instance. After drainPeriod (default 15s), the server initiates an
// http.Server.Shutdown(ctx) bounded by shutdownTimeout (default 25s).
//
// /health remains 200 throughout — it's the liveness probe; the kill
// signal comes from the process supervisor, not from /health flipping.
//
// Without this option the server uses Go's default signal handling
// (immediate shutdown, in-flight requests get a connection reset).
//
// The recommended drain + shutdown budget must be SMALLER than
// docker-compose's stop_grace_period (10s default; bump it to 45s in
// docker-compose.yml: `stop_grace_period: 45s` to give this room).
//
// Pass zero for either argument to use the default value.
func WithGracefulDrain(drainPeriod, shutdownTimeout time.Duration) Option {
	if drainPeriod <= 0 {
		drainPeriod = DefaultDrainPeriod
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = DefaultShutdownTimeout
	}
	return func(s *Server) {
		s.drain = &drainConfig{
			drainPeriod:     drainPeriod,
			shutdownTimeout: shutdownTimeout,
			impatientCh:     make(chan struct{}),
			shutdownDone:    make(chan struct{}),
		}
	}
}

// mountReadyz installs the /readyz endpoint. It's installed
// unconditionally (whether or not WithGracefulDrain is configured) so
// load balancers can probe a uniform path across the fleet. When drain
// is not configured the handler always returns 200; when configured it
// returns 503 once BeginDrain has been called.
func mountReadyz(s *Server) {
	s.Mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.drain != nil && s.drain.draining.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "draining",
				"service": s.Config.AppName,
				"version": s.Config.Version,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ready",
			"service": s.Config.AppName,
			"version": s.Config.Version,
		})
	})
}

// BeginDrain triggers the graceful-drain sequence. Returns immediately
// after flipping the readiness flag; the drain period and shutdown run
// in a background goroutine. Calling BeginDrain a second time signals
// "impatience" — the drain-period sleep is aborted and shutdown begins
// immediately. Safe to call from any goroutine.
//
// If WithGracefulDrain was not configured this is a no-op (the server
// will shut down via the default os.Interrupt handling instead). Tests
// can configure the drain explicitly and exercise this method to
// verify behavior without sending real signals.
func (s *Server) BeginDrain() {
	if s.drain == nil {
		return
	}
	// First call: flip draining + kick off the drain goroutine.
	// Second call: flip impatient + close impatientCh (idempotent via
	// sync.Once) so the drain goroutine short-circuits the sleep.
	if s.drain.draining.CompareAndSwap(false, true) {
		go s.runDrain()
		return
	}
	s.drain.impatient.Store(true)
	s.drain.once.Do(func() { close(s.drain.impatientCh) })
}

// runDrain is the drain-then-shutdown goroutine. It sleeps for the
// configured drain period (cancellable via impatientCh), then invokes
// shutdownFn with the shutdownTimeout budget. Always closes
// shutdownDone before returning so tests + Start() can synchronise.
func (s *Server) runDrain() {
	d := s.drain
	defer d.shutdownOnce.Do(func() { close(d.shutdownDone) })

	log.Printf("server: graceful drain begin (%s)", d.drainPeriod)

	timer := time.NewTimer(d.drainPeriod)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-d.impatientCh:
		log.Printf("server: second signal received, draining impatiently")
	}

	if d.shutdownFn == nil {
		// Drain triggered before Start() wired the shutdown closure
		// (only happens in tests that call BeginDrain on a server
		// that never ran). Nothing to do.
		return
	}

	d.shuttingDown.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer cancel()

	if err := d.shutdownFn(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server: shutdown returned error: %v", err)
	}
}

// DrainDone returns a channel that closes once the drain goroutine has
// finished its shutdown call. Returns nil when drain is not configured
// (callers must guard the nil receive — a nil channel blocks forever).
// Mainly useful in tests to assert the drain ran to completion without
// racing on internal state.
func (s *Server) DrainDone() <-chan struct{} {
	if s.drain == nil {
		return nil
	}
	return s.drain.shutdownDone
}

// SetShutdownFnForTest wires a custom shutdown closure into the drain
// state without going through Start(). Tests use this to attach an
// httptest.Server's shutdown, exercise BeginDrain, and assert the
// closure ran with the expected timeout budget. Returns false if drain
// was not configured. Not for production use.
func (s *Server) SetShutdownFnForTest(fn func(ctx context.Context) error) bool {
	if s.drain == nil {
		return false
	}
	s.drain.shutdownFn = fn
	return true
}

// IsDrainingForTest reports whether BeginDrain has flipped the
// readiness flag. Test-only helper that avoids exposing the internal
// atomic.
func (s *Server) IsDrainingForTest() bool {
	if s.drain == nil {
		return false
	}
	return s.drain.draining.Load()
}

// installSignalHandler wires SIGTERM + SIGINT to BeginDrain. Called
// from Start() only when drain is configured; without drain Go's
// default signal disposition (process exit) applies.
//
// The returned stop function detaches the signal handler — used by
// tests so the test process doesn't keep the signal channel hot.
func (s *Server) installSignalHandler() func() {
	if s.drain == nil {
		return func() {}
	}
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-sigCh:
				s.BeginDrain()
			case <-s.drain.shutdownDone:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		// Wake the goroutine if it's still blocked.
		select {
		case <-s.drain.shutdownDone:
		default:
			s.drain.shutdownOnce.Do(func() { close(s.drain.shutdownDone) })
		}
		<-done
	}
}

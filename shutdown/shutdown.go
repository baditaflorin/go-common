// Package shutdown provides a single-call graceful shutdown helper for
// net/http servers. It catches SIGTERM and SIGINT, stops accepting new
// connections, waits for in-flight requests to complete, then exits.
//
// Every production Go HTTP service needs graceful shutdown. Without it
// a SIGTERM during a deploy kills in-flight requests mid-response,
// causing 5xx errors visible to clients. This package encapsulates the
// canonical net/http.Server.Shutdown pattern so each service doesn't
// re-implement it.
//
// Usage (replaces log.Fatal(srv.ListenAndServe())):
//
//	srv := &http.Server{Addr: ":8080", Handler: myHandler}
//	log.Fatal(shutdown.ListenAndServe(srv))
//
// With options:
//
//	log.Fatal(shutdown.ListenAndServe(srv,
//	    shutdown.WithDrainTimeout(15*time.Second),
//	    shutdown.WithNotify(func(sig os.Signal) {
//	        log.Printf("shutdown signal: %v", sig)
//	    }),
//	))
//
// The function returns nil on clean shutdown, or the server error if
// it fails for any reason other than http.ErrServerClosed.
package shutdown

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// DefaultDrainTimeout is the time allowed for in-flight requests to
// complete before the server forcefully closes connections. The default
// is conservative (30s) because fleet services may have long-lived SSE
// or streaming connections. Override with WithDrainTimeout.
const DefaultDrainTimeout = 30 * time.Second

// Options controls shutdown behaviour.
type Options struct {
	DrainTimeout time.Duration
	Notify       func(os.Signal)
	ExtraSignals []os.Signal
}

// Option is a functional option for ListenAndServe.
type Option func(*Options)

// WithDrainTimeout sets how long to wait for in-flight requests to
// finish before forcing closure. Recommended values: 5–30s.
func WithDrainTimeout(d time.Duration) Option {
	return func(o *Options) { o.DrainTimeout = d }
}

// WithNotify sets a callback invoked when a shutdown signal is received.
// Use it to log the signal, emit a metric, or begin pre-shutdown work
// (e.g. deregistering from a service registry).
func WithNotify(fn func(os.Signal)) Option {
	return func(o *Options) { o.Notify = fn }
}

// WithSignals adds OS signals to listen for in addition to SIGTERM and
// SIGINT. Useful for SIGUSR1-triggered reload scenarios.
func WithSignals(sigs ...os.Signal) Option {
	return func(o *Options) { o.ExtraSignals = append(o.ExtraSignals, sigs...) }
}

// ListenAndServe starts srv and blocks until a SIGTERM or SIGINT is
// received, then performs a graceful shutdown with the configured drain
// timeout. Returns nil on clean shutdown, or the ListenAndServe error
// on startup failure.
//
// This is a drop-in replacement for log.Fatal(srv.ListenAndServe()).
func ListenAndServe(srv *http.Server, opts ...Option) error {
	o := &Options{DrainTimeout: DefaultDrainTimeout}
	for _, opt := range opts {
		opt(o)
	}

	sigs := make([]os.Signal, 0, 2+len(o.ExtraSignals))
	sigs = append(sigs, syscall.SIGTERM, syscall.SIGINT)
	sigs = append(sigs, o.ExtraSignals...)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sigs...)
	defer signal.Stop(sigCh)

	// Start serving in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	// Wait for a signal or a startup error.
	select {
	case err := <-errCh:
		// Startup failed before any signal.
		return err
	case sig := <-sigCh:
		if o.Notify != nil {
			o.Notify(sig)
		} else {
			slog.Info("shutdown signal received", "signal", sig.String())
		}
	}

	// Graceful drain.
	ctx, cancel := context.WithTimeout(context.Background(), o.DrainTimeout)
	defer cancel()

	slog.Info("shutting down server", "drain_timeout", o.DrainTimeout.String())
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown error", "error", err)
		return err
	}
	slog.Info("server stopped cleanly")

	// Drain any startup error that arrived after the signal.
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	default:
	}
	return nil
}

// ListenAndServeTLS is the TLS variant of ListenAndServe.
func ListenAndServeTLS(srv *http.Server, certFile, keyFile string, opts ...Option) error {
	o := &Options{DrainTimeout: DefaultDrainTimeout}
	for _, opt := range opts {
		opt(o)
	}

	sigs := make([]os.Signal, 0, 2+len(o.ExtraSignals))
	sigs = append(sigs, syscall.SIGTERM, syscall.SIGINT)
	sigs = append(sigs, o.ExtraSignals...)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sigs...)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		if o.Notify != nil {
			o.Notify(sig)
		} else {
			slog.Info("shutdown signal received (TLS)", "signal", sig.String())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.DrainTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return err
	}
	return nil
}

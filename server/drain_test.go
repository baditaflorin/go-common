package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/server"
)

// newDrainServer builds a Server with WithGracefulDrain configured and
// returns it alongside an httptest.Server that wraps the same handler
// chain. The drain config's shutdownFn is wired to the httptest
// server's Close so BeginDrain drives a real listener shutdown.
func newDrainServer(t *testing.T, drainPeriod, shutdownTimeout time.Duration) (*server.Server, *httptest.Server) {
	t.Helper()
	srv := server.New(
		&config.Config{AppName: "drain_test", Version: "0.0.0", Port: "0"},
		server.WithGracefulDrain(drainPeriod, shutdownTimeout),
	)
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	t.Cleanup(ts.Close)

	// Wire the shutdown closure to the httptest server's underlying
	// http.Server so BeginDrain.shutdownFn actually closes a listener.
	httpSrv := ts.Config
	srv.SetShutdownFnForTest(httpSrv.Shutdown)
	return srv, ts
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// TestDrain_BeginDrain_ReadyzReturns503 — once BeginDrain has been
// called, /readyz flips from 200 to 503 so the load balancer drains
// traffic away from this instance.
func TestDrain_BeginDrain_ReadyzReturns503(t *testing.T) {
	srv, ts := newDrainServer(t, 5*time.Second, 5*time.Second)

	if code, _ := get(t, ts.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("pre-drain /readyz: got %d, want 200", code)
	}

	srv.BeginDrain()

	if code, body := get(t, ts.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("post-drain /readyz: got %d body=%q, want 503", code, body)
	}
}

// TestDrain_HealthStaysHealthyDuringDrain — /health is the liveness
// probe; it must keep returning 200 throughout the drain so the
// process supervisor does not kill us before drain completes.
func TestDrain_HealthStaysHealthyDuringDrain(t *testing.T) {
	srv, ts := newDrainServer(t, 5*time.Second, 5*time.Second)
	srv.BeginDrain()

	if code, _ := get(t, ts.URL+"/health"); code != http.StatusOK {
		t.Fatalf("/health during drain: got %d, want 200", code)
	}
}

// TestDrain_DrainPeriodElapses_ServerShutsDown — after the configured
// drain period elapses, the drain goroutine invokes shutdownFn and
// the listener stops accepting new connections.
func TestDrain_DrainPeriodElapses_ServerShutsDown(t *testing.T) {
	srv, _ := newDrainServer(t, 100*time.Millisecond, 2*time.Second)
	srv.BeginDrain()

	select {
	case <-srv.DrainDone():
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not complete within 2s")
	}
}

// TestDrain_InFlightRequestsComplete — a request in flight when
// BeginDrain fires must complete (not be reset). The drain period
// gives in-flight handlers time to finish before Shutdown is called.
func TestDrain_InFlightRequestsComplete(t *testing.T) {
	srv := server.New(
		&config.Config{AppName: "drain_test", Version: "0.0.0", Port: "0"},
		server.WithGracefulDrain(300*time.Millisecond, 2*time.Second),
	)
	srv.Mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	t.Cleanup(ts.Close)
	srv.SetShutdownFnForTest(ts.Config.Shutdown)

	done := make(chan int, 1)
	go func() {
		resp, err := http.Get(ts.URL + "/slow")
		if err != nil {
			done <- -1
			return
		}
		defer resp.Body.Close()
		done <- resp.StatusCode
	}()

	// Trigger drain mid-flight (well before the handler's 100ms sleep
	// completes), then wait for the slow request.
	time.Sleep(20 * time.Millisecond)
	srv.BeginDrain()

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("in-flight request: got %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}
}

// TestDrain_SecondSignalImpatient — a second BeginDrain call after
// the first short-circuits the drain-period sleep and triggers
// shutdown immediately.
func TestDrain_SecondSignalImpatient(t *testing.T) {
	// 5s drain period — if impatience does NOT work, the test fails
	// by timing out at 1s.
	srv, _ := newDrainServer(t, 5*time.Second, 2*time.Second)

	srv.BeginDrain()
	// Give the drain goroutine a moment to enter its sleep.
	time.Sleep(50 * time.Millisecond)
	srv.BeginDrain()

	select {
	case <-srv.DrainDone():
		// Expected: impatience cut the 5s sleep short.
	case <-time.After(1 * time.Second):
		t.Fatal("second BeginDrain did not short-circuit the drain period")
	}
}

// TestDrain_ShutdownTimeoutBudget — when a handler blocks forever,
// http.Server.Shutdown returns once the ctx deadline elapses. The
// drain goroutine completes after roughly drainPeriod + shutdownTimeout
// regardless of misbehaving handlers.
func TestDrain_ShutdownTimeoutBudget(t *testing.T) {
	srv := server.New(
		&config.Config{AppName: "drain_test", Version: "0.0.0", Port: "0"},
		server.WithGracefulDrain(50*time.Millisecond, 200*time.Millisecond),
	)
	hang := make(chan struct{})
	srv.Mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		<-hang
	})
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	// Cleanup order is LIFO: unblock the hang first, then close the
	// test server (otherwise ts.Close blocks waiting for the active
	// TCP connection running the /hang handler).
	t.Cleanup(func() {
		close(hang)
		ts.CloseClientConnections()
		ts.Close()
	})
	srv.SetShutdownFnForTest(ts.Config.Shutdown)

	// Kick off a request that will hang past the shutdown timeout.
	go func() {
		resp, err := http.Get(ts.URL + "/hang")
		if err == nil {
			resp.Body.Close()
		}
	}()
	// Wait until the request is in-flight on the server side.
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	srv.BeginDrain()

	select {
	case <-srv.DrainDone():
		elapsed := time.Since(start)
		// drainPeriod (50ms) + shutdownTimeout (200ms) = ~250ms.
		// Allow 1s headroom for CI jitter. The key assertion: drain
		// did NOT hang waiting for the blocked handler.
		if elapsed > 1*time.Second {
			t.Fatalf("drain took %s, want < 1s (shutdown timeout should have triggered)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain hung past shutdown timeout budget")
	}
}

// TestDrain_NoDrainConfigured_DefaultsToImmediate — a Server built
// WITHOUT WithGracefulDrain does not flip /readyz under BeginDrain
// (the method is a no-op) and does not expose drain state.
func TestDrain_NoDrainConfigured_DefaultsToImmediate(t *testing.T) {
	srv := server.New(&config.Config{AppName: "no_drain", Version: "0.0.0", Port: "0"})
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	t.Cleanup(ts.Close)

	if code, _ := get(t, ts.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz with no drain: got %d, want 200", code)
	}

	// No-op — must not panic and must not flip anything.
	srv.BeginDrain()

	if srv.IsDrainingForTest() {
		t.Fatal("BeginDrain on a non-drain server should be a no-op")
	}
	if code, _ := get(t, ts.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz after BeginDrain (no drain configured): got %d, want 200", code)
	}
	if ch := srv.DrainDone(); ch != nil {
		t.Fatal("DrainDone should be nil when WithGracefulDrain is not configured")
	}
}

// TestDrain_ReadyzDefaultExists — the /readyz endpoint is installed
// unconditionally so the route is uniform across the fleet. A server
// without WithGracefulDrain still serves it, always returning 200.
func TestDrain_ReadyzDefaultExists(t *testing.T) {
	srv := server.New(&config.Config{AppName: "no_drain", Version: "0.0.0", Port: "0"})
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	t.Cleanup(ts.Close)

	for i := 0; i < 3; i++ {
		if code, _ := get(t, ts.URL+"/readyz"); code != http.StatusOK {
			t.Fatalf("readyz probe #%d: got %d, want 200", i, code)
		}
	}
}

// TestDrain_BeginDrainMultipleConcurrent — concurrent callers of
// BeginDrain do not double-trigger the drain goroutine or panic on
// double-close of the impatient channel. Guards the sync.Once + atomic
// CAS lattice.
func TestDrain_BeginDrainMultipleConcurrent(t *testing.T) {
	srv, _ := newDrainServer(t, 100*time.Millisecond, 1*time.Second)

	var wg atomic.Int32
	wg.Store(20)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			srv.BeginDrain()
			if wg.Add(-1) == 0 {
				close(done)
			}
		}()
	}

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("concurrent BeginDrain calls hung")
	}

	select {
	case <-srv.DrainDone():
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not complete after concurrent triggers")
	}
}

// TestDrain_DefaultBudgets — passing zero (or negative) values uses
// the documented defaults.
func TestDrain_DefaultBudgets(t *testing.T) {
	srv := server.New(
		&config.Config{AppName: "drain_defaults", Version: "0.0.0", Port: "0"},
		server.WithGracefulDrain(0, 0),
	)
	// Wire a no-op shutdownFn so BeginDrain doesn't crash. We only
	// assert that BeginDrain flips the readiness flag and the drain
	// goroutine runs — the default 15s wait would take too long to
	// run to completion in unit tests, so we don't wait for it.
	srv.SetShutdownFnForTest(func(ctx context.Context) error { return nil })

	srv.BeginDrain()
	if !srv.IsDrainingForTest() {
		t.Fatal("BeginDrain should flip the readiness flag with default budgets")
	}
	// Make the drain impatient so the goroutine completes promptly
	// instead of sitting on the 15s default sleep.
	srv.BeginDrain()
	select {
	case <-srv.DrainDone():
	case <-time.After(1 * time.Second):
		t.Fatal("impatient drain with default budgets did not complete")
	}
}

package server_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/server"
)

// newCancellationServer spins up a test server with the cancellation
// option installed plus a single registered handler. The handler runs
// the provided fn under the request's (cancellable) context. Returns
// the live test server and a cleanup func.
func newCancellationServer(t *testing.T, cfg server.CancellationConfig, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := server.New(
		&config.Config{AppName: "cancel-test", Version: "0.0.0", Port: "0"},
		server.WithCancellationRegistry(cfg),
	)
	srv.Mux.HandleFunc("/work", handler)
	ts := httptest.NewServer(middleware.Chain(srv.Mux, srv.Middlewares...))
	t.Cleanup(ts.Close)
	return ts
}

// TestCancellation_RequestRegisteredWhileInFlight verifies that an
// in-flight request appears in the registry (observable via the side
// effect that a DELETE against its id cancels it) and is removed
// after the handler returns (subsequent DELETE returns 404).
func TestCancellation_RequestRegisteredWhileInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	const id = "req-inflight"
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", id)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	// While in-flight: cancel must succeed.
	resp, err := doDelete(t, ts.URL+"/cancel/"+id)
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 while in-flight, got %d", resp.StatusCode)
	}

	// The handler may still be blocked on `release`; unblock it.
	close(release)
	<-done

	// After completion: cancel must 404 (entry removed on cancel above
	// AND on handler return).
	resp2, err := doDelete(t, ts.URL+"/cancel/"+id)
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after completion, got %d", resp2.StatusCode)
	}
}

// TestCancellation_DELETEEndpointCancels confirms the handler observes
// ctx.Done() once the cancel DELETE arrives.
func TestCancellation_DELETEEndpointCancels(t *testing.T) {
	started := make(chan struct{})
	var cancelled atomic.Bool
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
			w.WriteHeader(http.StatusRequestTimeout)
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})

	const id = "req-cancel"
	go func() {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", id)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	resp, err := doDelete(t, ts.URL+"/cancel/"+id)
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cancelled.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("handler did not observe ctx.Done() within timeout")
}

// TestCancellation_DELETEReturns404IfNoMatch — a cancel against an
// unknown id is a 404.
func TestCancellation_DELETEReturns404IfNoMatch(t *testing.T) {
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	resp, err := doDelete(t, ts.URL+"/cancel/unknown-id")
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestCancellation_DoubleCancel404OnSecond — first cancel returns 204,
// second cancel against the same id returns 404 (registry entry was
// removed on the first call).
func TestCancellation_DoubleCancel404OnSecond(t *testing.T) {
	started := make(chan struct{})
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		w.WriteHeader(http.StatusRequestTimeout)
	})

	const id = "req-double"
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", id)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	r1, _ := doDelete(t, ts.URL+"/cancel/"+id)
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("first cancel: expected 204, got %d", r1.StatusCode)
	}
	<-done

	r2, _ := doDelete(t, ts.URL+"/cancel/"+id)
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("second cancel: expected 404, got %d", r2.StatusCode)
	}
}

// TestCancellation_AutoGeneratedRequestIDInResponseHeader — request
// without X-Request-Id receives one in the response.
func TestCancellation_AutoGeneratedRequestIDInResponseHeader(t *testing.T) {
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	resp, err := http.Get(ts.URL + "/work")
	if err != nil {
		t.Fatalf("get err: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Fatalf("expected auto-generated X-Request-Id in response, got empty")
	}
}

// TestCancellation_ProvidedRequestIDHonored — caller-supplied
// X-Request-Id is the key used by /cancel/<id>.
func TestCancellation_ProvidedRequestIDHonored(t *testing.T) {
	started := make(chan struct{})
	var observed atomic.Bool
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		observed.Store(true)
		w.WriteHeader(http.StatusRequestTimeout)
	})

	const id = "foo"
	go func() {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", id)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	resp, err := doDelete(t, ts.URL+"/cancel/foo")
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if observed.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("handler did not observe cancellation for caller-supplied id")
}

// TestCancellation_MaxInFlightDoesNotBlock — fill the registry; new
// requests still get served (just without cancel registration).
func TestCancellation_MaxInFlightDoesNotBlock(t *testing.T) {
	const cap = 3
	gate := make(chan struct{})
	var inflight atomic.Int32
	var served atomic.Int32
	ts := newCancellationServer(t, server.CancellationConfig{MaxInFlight: cap}, func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		<-gate
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	var wg sync.WaitGroup
	// Launch cap requests to fill the registry.
	for i := 0; i < cap; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
			req.Header.Set("X-Request-Id", "fill-"+itoa(i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("fill request err: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}

	// Wait until the registry is full.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && inflight.Load() < int32(cap) {
		time.Sleep(5 * time.Millisecond)
	}
	if inflight.Load() != int32(cap) {
		t.Fatalf("expected %d in-flight, got %d", cap, inflight.Load())
	}

	// One more request — registry is full, but it must still be
	// served (graceful degradation, no block, no 503).
	wg.Add(1)
	overflowDone := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(overflowDone)
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", "overflow")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("overflow request err: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("overflow request: expected 200, got %d", resp.StatusCode)
		}
	}()

	// Give the overflow request a moment to enter the handler.
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && inflight.Load() < int32(cap)+1 {
		time.Sleep(5 * time.Millisecond)
	}
	if inflight.Load() != int32(cap)+1 {
		t.Fatalf("expected overflow request to enter handler; in-flight=%d", inflight.Load())
	}

	// Confirm the overflow request was NOT registered: cancel against
	// its id returns 404.
	resp, _ := doDelete(t, ts.URL+"/cancel/overflow")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("overflow request should not be cancellable; expected 404, got %d", resp.StatusCode)
	}

	// Release all handlers and wait for completion.
	close(gate)
	wg.Wait()
	if served.Load() != int32(cap)+1 {
		t.Fatalf("expected %d served, got %d", cap+1, served.Load())
	}
}

// TestCancellation_AdminGateRefusesNonAdmin — AdminGate returning an
// error responds 401 and skips cancellation.
func TestCancellation_AdminGateRefusesNonAdmin(t *testing.T) {
	gate := func(r *http.Request) error {
		if r.Header.Get(header.AdminToken) != "secret" {
			return errors.New("forbidden")
		}
		return nil
	}
	started := make(chan struct{})
	ts := newCancellationServer(t, server.CancellationConfig{AdminGate: gate}, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		w.WriteHeader(http.StatusRequestTimeout)
	})

	const id = "guarded"
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/work", nil)
		req.Header.Set("X-Request-Id", id)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	// No admin token — should be 401, request still in flight.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/cancel/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	// With the admin token, the cancel must succeed.
	req2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/cancel/"+id, nil)
	req2.Header.Set(header.AdminToken, "secret")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("delete err: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 with admin token, got %d", resp2.StatusCode)
	}
	<-done
}

// TestCancellation_NonDELETEMethodRejected — only DELETE is honored.
func TestCancellation_NonDELETEMethodRejected(t *testing.T) {
	ts := newCancellationServer(t, server.CancellationConfig{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	resp, err := http.Get(ts.URL + "/cancel/anything")
	if err != nil {
		t.Fatalf("get err: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

// doDelete is a small helper to issue DELETE requests in tests.
func doDelete(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp, nil
}

// itoa is a small helper to avoid pulling strconv for one int->string.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

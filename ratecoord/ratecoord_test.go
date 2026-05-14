package ratecoord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitRemoteHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wait" {
			http.Error(w, "wrong path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{"waited_ms": 42})
	}))
	defer srv.Close()
	t.Setenv("RATECOORD_URL", srv.URL)
	t.Setenv("RATECOORD_API_KEY", "test-key")

	c := New()
	res, err := c.Wait(context.Background(), "example.com", 1, time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.FellBack {
		t.Fatalf("should not have fallen back: %+v", res)
	}
	if res.WaitedMs != 42 {
		t.Fatalf("expected waited_ms=42, got %d", res.WaitedMs)
	}
}

func TestWaitFallsBackOnCoordinatorDown(t *testing.T) {
	t.Setenv("RATECOORD_URL", "http://127.0.0.1:1") // closed
	t.Setenv("RATECOORD_API_KEY", "test-key")
	t.Setenv("RATECOORD_DEFAULT_RPS", "100")
	t.Setenv("RATECOORD_DEFAULT_BURST", "10")

	c := New()
	res, err := c.Wait(context.Background(), "example.com", 1, time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !res.FellBack {
		t.Fatalf("expected FellBack=true, got %+v", res)
	}
}

func TestWaitFallsBackThrottles(t *testing.T) {
	t.Setenv("RATECOORD_URL", "http://127.0.0.1:1")
	t.Setenv("RATECOORD_API_KEY", "test-key")
	t.Setenv("RATECOORD_DEFAULT_RPS", "2") // 2 tokens/sec
	t.Setenv("RATECOORD_DEFAULT_BURST", "1")

	c := New()
	// First call returns immediately (burst available)
	if _, err := c.Wait(context.Background(), "host.example", 1, time.Second); err != nil {
		t.Fatal(err)
	}
	// Second call should wait ~500ms (1/rps)
	start := time.Now()
	if _, err := c.Wait(context.Background(), "host.example", 1, time.Second); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 350*time.Millisecond {
		t.Fatalf("expected throttling, got %v", elapsed)
	}
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	t.Setenv("RATECOORD_URL", srv.URL)
	c := New()
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	t.Setenv("RATECOORD_URL", "http://127.0.0.1:1")
	c2 := New()
	if err := c2.Probe(context.Background()); err == nil {
		t.Fatalf("probe should fail on closed port")
	}
}

func TestWaitMissingHost(t *testing.T) {
	c := New()
	if _, err := c.Wait(context.Background(), "", 1, time.Second); err == nil {
		t.Fatalf("empty host should error")
	}
}

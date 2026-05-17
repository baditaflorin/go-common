package selftest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewSuite_Empty — zero checks renders ok=true, pass=0, fail=0,
// http 200.
func TestNewSuite_Empty(t *testing.T) {
	s := NewSuite("svc", "1.0.0")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/selftest", nil)
	s.Render(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("want Content-Type application/json, got %q", got)
	}

	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if !resp.OK || resp.Pass != 0 || resp.Fail != 0 || len(resp.Checks) != 0 {
		t.Errorf("want ok=true pass=0 fail=0 checks=[], got %+v", resp)
	}
	if resp.Service != "svc" || resp.Version != "1.0.0" {
		t.Errorf("service/version not echoed: %+v", resp)
	}
}

// TestCheck_RegisterAndRun — one fail + two pass — counts add up,
// http 503, ok=false, the failing check's err is in the JSON.
func TestCheck_RegisterAndRun(t *testing.T) {
	s := NewSuite("svc", "1.0.0")
	s.Check("a", func(ctx context.Context) error { return nil })
	s.Check("b", func(ctx context.Context) error { return errors.New("boom") })
	s.Check("c", func(ctx context.Context) error { return nil })

	rr := httptest.NewRecorder()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if resp.OK || resp.Pass != 2 || resp.Fail != 1 {
		t.Errorf("want ok=false pass=2 fail=1, got %+v", resp)
	}
	if len(resp.Checks) != 3 || resp.Checks[1].Name != "b" || resp.Checks[1].Pass || resp.Checks[1].Err != "boom" {
		t.Errorf("checks slice off: %+v", resp.Checks)
	}
	// Registration order preserved.
	if resp.Checks[0].Name != "a" || resp.Checks[2].Name != "c" {
		t.Errorf("registration order not preserved: %+v", resp.Checks)
	}
}

// TestCheck_AllPass — three passing checks — http 200, ok=true.
func TestCheck_AllPass(t *testing.T) {
	s := NewSuite("svc", "1.0.0")
	for _, n := range []string{"x", "y", "z"} {
		n := n
		s.Check(n, func(ctx context.Context) error { _ = n; return nil })
	}

	rr := httptest.NewRecorder()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if !resp.OK || resp.Pass != 3 || resp.Fail != 0 {
		t.Errorf("want ok=true pass=3 fail=0, got %+v", resp)
	}
}

// TestCheck_Timeout — a check that ignores its context and sleeps
// longer than the timeout reports {pass:false, err:"timeout after
// 200ms"} and the response is still 503. Uses a short custom
// timeout so the test doesn't pin the suite for 5 s.
func TestCheck_Timeout(t *testing.T) {
	s := NewSuite("svc", "1.0.0", WithCheckTimeout(200*time.Millisecond))
	s.Check("slow", func(ctx context.Context) error {
		// Deliberately ignore ctx — simulates a runaway upstream call.
		time.Sleep(400 * time.Millisecond)
		return nil
	})

	rr := httptest.NewRecorder()
	start := time.Now()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))
	elapsed := time.Since(start)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	// We should return ~at the timeout, NOT after the sleep finishes.
	// Generous upper bound to absorb scheduler jitter in CI.
	if elapsed > 350*time.Millisecond {
		t.Errorf("Render blocked past timeout (%s) — watchdog not firing", elapsed)
	}

	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if len(resp.Checks) != 1 || resp.Checks[0].Pass {
		t.Fatalf("want one failing check, got %+v", resp.Checks)
	}
	if !strings.Contains(resp.Checks[0].Err, "timeout") {
		t.Errorf("want err to mention timeout, got %q", resp.Checks[0].Err)
	}
}

// TestCheck_TimeoutHonoringContext — a well-behaved check that
// returns ctx.Err() when its context fires also reports as a failed
// check (the err carries the context.DeadlineExceeded message).
func TestCheck_TimeoutHonoringContext(t *testing.T) {
	s := NewSuite("svc", "1.0.0", WithCheckTimeout(100*time.Millisecond))
	s.Check("polite", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	rr := httptest.NewRecorder()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if len(resp.Checks) != 1 || resp.Checks[0].Pass {
		t.Fatalf("want one failing check, got %+v", resp.Checks)
	}
}

// TestCheck_Panic — a panicking check becomes a failed check, the
// suite does not crash, http 503.
func TestCheck_Panic(t *testing.T) {
	s := NewSuite("svc", "1.0.0")
	s.Check("panicky", func(ctx context.Context) error {
		panic("kaboom")
	})
	s.Check("ok", func(ctx context.Context) error { return nil })

	rr := httptest.NewRecorder()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	var resp Response
	mustUnmarshal(t, rr.Body.Bytes(), &resp)
	if resp.Pass != 1 || resp.Fail != 1 {
		t.Errorf("want pass=1 fail=1, got %+v", resp)
	}
	if !strings.Contains(resp.Checks[0].Err, "panic") {
		t.Errorf("want err to mention panic, got %q", resp.Checks[0].Err)
	}
}

// TestRender_Concurrent — 100 concurrent renders, each running the
// same three checks. Counters guarantee every check ran the
// expected number of times and the responses are well-formed.
// Combined with `go test -race` this catches shared-mutable-state
// regressions in Render / run.
func TestRender_Concurrent(t *testing.T) {
	s := NewSuite("svc", "1.0.0")
	var calls atomic.Int64
	for i := 0; i < 3; i++ {
		s.Check("c", func(ctx context.Context) error {
			calls.Add(1)
			return nil
		})
	}

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))
			if rr.Code != http.StatusOK {
				t.Errorf("want 200, got %d", rr.Code)
			}
			var resp Response
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Errorf("unmarshal: %v", err)
				return
			}
			if !resp.OK || resp.Pass != 3 {
				t.Errorf("bad resp: %+v", resp)
			}
		}()
	}
	wg.Wait()

	if got, want := calls.Load(), int64(n*3); got != want {
		t.Errorf("call count: got %d want %d", got, want)
	}
}

// TestRender_JSONShape — verifies the exact field names + types
// against the canonical shape, using a golden file as the spec.
// Failing this means a consumer (the aggregator) might break.
func TestRender_JSONShape(t *testing.T) {
	s := NewSuite("svc", "1.2.3")
	s.Check("alpha", func(ctx context.Context) error { return nil })
	s.Check("beta", func(ctx context.Context) error { return errors.New("nope") })

	rr := httptest.NewRecorder()
	s.Render(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))

	// Decode into a generic map so we can assert against exact field
	// names + types without our own struct tags lying to us.
	var got map[string]any
	mustUnmarshal(t, rr.Body.Bytes(), &got)

	// duration_ms is wall-clock — normalise to 0 before comparing
	// against the golden.
	got["duration_ms"] = float64(0)

	goldenPath := filepath.Join("testdata", "render_shape.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		writeGolden(t, goldenPath, got)
	}
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want map[string]any
	mustUnmarshal(t, wantBytes, &want)

	gotCanon := mustMarshal(t, got)
	wantCanon := mustMarshal(t, want)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("shape mismatch:\n got=%s\nwant=%s", gotCanon, wantCanon)
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, string(b))
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func writeGolden(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, mustMarshal(t, v), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

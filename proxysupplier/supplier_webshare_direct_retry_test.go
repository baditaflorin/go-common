package proxysupplier

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetries shrinks the retry backoff for the duration of a test.
func fastRetries(t *testing.T, base, max time.Duration) {
	t.Helper()
	ob, om := webshareDirectRetryBase, webshareDirectRetryMax
	webshareDirectRetryBase, webshareDirectRetryMax = base, max
	t.Cleanup(func() { webshareDirectRetryBase, webshareDirectRetryMax = ob, om })
}

// flakyWebshare answers the first `failures` requests with status (and Retry-After when
// non-empty), then serves a valid pool of n entries. It records every request time.
type flakyWebshare struct {
	*httptest.Server
	mu    sync.Mutex
	times []time.Time
}

func newFlakyWebshare(t *testing.T, failures int, status int, retryAfter string, n int) *flakyWebshare {
	t.Helper()
	f := &flakyWebshare{}
	entries := make([]webshareEntry, n)
	for i := range entries {
		entries[i] = webshareEntry{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5000 + i, Valid: true}
	}
	var calls atomic.Int32
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.times = append(f.times, time.Now())
		f.mu.Unlock()
		if int(calls.Add(1)) <= failures {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			http.Error(w, "throttled", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: n, Results: entries})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *flakyWebshare) requests() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.times...)
}

// newTestSupplier builds a webshare_direct supplier and stops its refresh loop when the test ends.
func newTestSupplier(t *testing.T) Supplier {
	t.Helper()
	s := NewFromConfig(Config{Supplier: "webshare_direct", WebshareAPIKey: "test-key"})
	if ws, ok := s.(*webshareDirectSupplier); ok {
		t.Cleanup(ws.Close)
	}
	return s
}

func snapshotOf(t *testing.T, s Supplier) WebshareDirectPoolState {
	t.Helper()
	ws, ok := s.(*webshareDirectSupplier)
	if !ok {
		t.Fatalf("supplier is %T, want *webshareDirectSupplier", s)
	}
	return ws.poolSnapshot(time.Now().UnixNano())
}

// The incident: the very first pool fetch fails (the Webshare API throttled several replicas that
// restarted together). The pool used to stay empty for a whole refresh interval (10 minutes).
func TestWebshareDirect_FailedInitialFetchIsRetriedWithBackoff(t *testing.T) {
	fastRetries(t, 5*time.Millisecond, 40*time.Millisecond)
	srv := newFlakyWebshare(t, 3, http.StatusTooManyRequests, "", 4)
	withWebshareDirectBaseURL(t, srv.URL)

	s := newTestSupplier(t)
	if u := waitForNonEmptyProxyURL(t, s, 3*time.Second); u == "" {
		t.Fatal("pool never filled")
	}
	if got := len(srv.requests()); got != 4 {
		t.Fatalf("API saw %d requests, want 3 failures + 1 success", got)
	}
	st := snapshotOf(t, s)
	if st.Total != 4 || st.RefreshFailures != 3 || st.RefreshSuccesses < 1 || st.ConsecutiveRefreshFailures != 0 {
		t.Fatalf("state after recovery = %+v", st)
	}
	if st.LastRefreshAgeSeconds < 0 || st.LastRefreshAgeSeconds > 5 {
		t.Fatalf("LastRefreshAgeSeconds = %v, want a small non-negative age", st.LastRefreshAgeSeconds)
	}
}

func TestWebshareDirect_RetriesCoverServerErrorsAndEmptyPools(t *testing.T) {
	for name, status := range map[string]int{"500": http.StatusInternalServerError, "503": http.StatusServiceUnavailable, "401 then fixed": http.StatusUnauthorized} {
		t.Run(name, func(t *testing.T) {
			fastRetries(t, 2*time.Millisecond, 10*time.Millisecond)
			srv := newFlakyWebshare(t, 2, status, "", 2)
			withWebshareDirectBaseURL(t, srv.URL)
			s := newTestSupplier(t)
			waitForNonEmptyProxyURL(t, s, 3*time.Second)
		})
	}

	t.Run("zero valid entries is a failure and is retried", func(t *testing.T) {
		fastRetries(t, 2*time.Millisecond, 10*time.Millisecond)
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			entries := []webshareEntry{{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5000, Valid: false}}
			if calls.Add(1) > 3 {
				entries[0].Valid = true
			}
			_ = json.NewEncoder(w).Encode(webshareListResponse{Count: 1, Results: entries})
		}))
		defer srv.Close()
		withWebshareDirectBaseURL(t, srv.URL)
		s := newTestSupplier(t)
		waitForNonEmptyProxyURL(t, s, 3*time.Second)
	})
}

// A failed refresh with a live pool keeps serving the previous list and retries soon, not after the full interval.
func TestWebshareDirect_FailedRefreshKeepsThePoolAndRetriesBeforeTheInterval(t *testing.T) {
	fastRetries(t, 5*time.Millisecond, 20*time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n >= 2 && n <= 4 { // the first refresh after the initial fetch fails three times
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: 2, Results: []webshareEntry{
			{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5000, Valid: true},
			{Username: "u", Password: "p", ProxyAddress: "10.0.0.1", Port: 5001, Valid: true},
		}})
	}))
	defer srv.Close()

	// 40 ms interval (a 30 s one would hide the bug in a unit test), jitter shrinks it to 36-44 ms.
	s := &webshareDirectSupplier{
		apiKey: "test-key", baseURL: srv.URL + "/", refreshInterval: 40 * time.Millisecond,
		retryBase: 5 * time.Millisecond, retryMax: 20 * time.Millisecond,
		httpClient: &http.Client{Timeout: time.Second}, stop: make(chan struct{}),
	}
	t.Cleanup(s.Close)
	go s.refreshLoop()

	waitForNonEmptyProxyURL(t, s, 2*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	sawFailure := false
	for time.Now().Before(deadline) {
		st := s.poolSnapshot(time.Now().UnixNano())
		if st.Total != 2 {
			t.Fatalf("pool dropped to %d entries during failed refreshes, want it kept at 2", st.Total)
		}
		if s.ProxyURL() == "" {
			t.Fatal("ProxyURL() empty while only a refresh was failing")
		}
		if st.ConsecutiveRefreshFailures > 0 {
			sawFailure = true
		}
		if sawFailure && st.ConsecutiveRefreshFailures == 0 {
			return // failed, retried, recovered, pool intact
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no failure-then-recovery observed (sawFailure=%v, state=%+v)", sawFailure, s.poolSnapshot(time.Now().UnixNano()))
}

// The API's own Retry-After is honoured (it is the API telling us when it will accept us again).
func TestWebshareDirect_RetryAfterIsHonoured(t *testing.T) {
	fastRetries(t, 2*time.Millisecond, 10*time.Millisecond)
	srv := newFlakyWebshare(t, 1, http.StatusTooManyRequests, "1", 2)
	withWebshareDirectBaseURL(t, srv.URL)
	s := newTestSupplier(t)
	waitForNonEmptyProxyURL(t, s, 5*time.Second)
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("API saw %d requests, want 2", len(reqs))
	}
	if gap := reqs[1].Sub(reqs[0]); gap < 900*time.Millisecond {
		t.Fatalf("retry came %s after a 429 with Retry-After: 1, want at least about 1 s", gap)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"7", 7 * time.Second},
		{"-3", 0},
		{"soon", 0},
		{"99999", webshareDirectMaxRetryAfter},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, now); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestWebshareDirect_NextDelay(t *testing.T) {
	mk := func(rnd float64, interval time.Duration) *webshareDirectSupplier {
		return &webshareDirectSupplier{refreshInterval: interval, retryBase: time.Second, retryMax: time.Minute, rand01: func() float64 { return rnd }}
	}
	errFetch := errors.New("boom")

	t.Run("success is the interval spread by plus or minus 10 percent", func(t *testing.T) {
		if got := mk(0, 10*time.Minute).nextDelay(nil, 0); got != 9*time.Minute {
			t.Errorf("rand 0: %s, want 9m", got)
		}
		if got := mk(0.5, 10*time.Minute).nextDelay(nil, 0); got != 10*time.Minute {
			t.Errorf("rand 0.5: %s, want 10m", got)
		}
		if got := mk(0.999999, 10*time.Minute).nextDelay(nil, 0); got < 10*time.Minute+59*time.Second || got > 11*time.Minute {
			t.Errorf("rand ~1: %s, want about 11m", got)
		}
	})

	t.Run("failures back off exponentially up to the cap, equal jitter", func(t *testing.T) {
		s := mk(0, 10*time.Minute) // rand 0 -> exactly half of the backoff
		want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
		for i, w := range want {
			if got := s.nextDelay(errFetch, i+1); got != w {
				t.Errorf("failure #%d (rand 0): %s, want %s", i+1, got, w)
			}
		}
		hi := mk(0.999999, 10*time.Minute) // rand ~1 -> the full backoff
		if got := hi.nextDelay(errFetch, 3); got < 3900*time.Millisecond || got > 4*time.Second {
			t.Errorf("failure #3 (rand ~1): %s, want about 4s", got)
		}
		if got := hi.nextDelay(errFetch, 50); got > time.Minute {
			t.Errorf("far into the failures: %s, want at most the 1 minute cap", got)
		}
	})

	t.Run("a retry is never later than the refresh interval", func(t *testing.T) {
		s := mk(0.999999, 5*time.Second)
		if got := s.nextDelay(errFetch, 20); got > 5*time.Second {
			t.Errorf("%s exceeds the 5 s interval", got)
		}
	})

	t.Run("Retry-After wins over a shorter backoff and is capped", func(t *testing.T) {
		s := mk(0, 10*time.Minute)
		if got := s.nextDelay(&webshareAPIError{Status: 429, RetryAfter: 45 * time.Second}, 1); got != 45*time.Second {
			t.Errorf("Retry-After 45 s: %s", got)
		}
		if got := s.nextDelay(&webshareAPIError{Status: 429, RetryAfter: time.Hour}, 1); got != 10*time.Minute {
			t.Errorf("an absurd Retry-After is capped to max(interval, 5 min): %s", got)
		}
		if got := s.nextDelay(&webshareAPIError{Status: 429}, 1); got != 500*time.Millisecond {
			t.Errorf("no Retry-After: %s, want the plain backoff", got)
		}
	})
}

func TestWebshareDirect_RefreshMetricsStartAtNeverAndAreZeroWithoutKey(t *testing.T) {
	s := &webshareDirectSupplier{}
	st := s.poolSnapshot(time.Now().UnixNano())
	if st.LastRefreshAgeSeconds != -1 || st.RefreshSuccesses != 0 || st.RefreshFailures != 0 || st.ConsecutiveRefreshFailures != 0 {
		t.Fatalf("fresh supplier state = %+v, want age -1 and zero counters", st)
	}
}

package fleetfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestArchiveUnavailablePreservesRetryAfterWithoutLiveFallback(t *testing.T) {
	for _, tc := range []struct{ name, body, retry string }{
		{"shed", `{"status":"error","error":{"code":503,"error_code":"load_shed","message":"busy"}}`, "7"},
		{"archive unavailable", `{"error":"archive unavailable"}`, "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"no hint", `{"error":"unavailable"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("source") != "commoncrawl" {
					t.Error("missing archive source")
				}
				if tc.retry != "" {
					w.Header().Set("Retry-After", tc.retry)
				}
				w.WriteHeader(503)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer cache.Close()
			var fallbacks atomic.Int32
			live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbacks.Add(1) }))
			defer live.Close()
			c := NewClient(WithCacheURL(cache.URL), WithSource(SourceCommonCrawl), WithFallbackClient(live.Client()))
			_, err := c.Get(context.Background(), "https://example.com/")
			var unavailable *ArchiveUnavailableError
			if !errors.Is(err, ErrArchiveUnavailable) || !errors.As(err, &unavailable) {
				t.Fatalf("expected typed archive unavailable, got %T %v", err, err)
			}
			if errors.Is(err, ErrRenderBusy) || unavailable.StatusCode != 503 || unavailable.RetryAfter != tc.retry {
				t.Fatalf("unexpected error %+v", unavailable)
			}
			if calls.Load() != 1 || fallbacks.Load() != 0 {
				t.Fatalf("cache calls=%d fallback calls=%d", calls.Load(), fallbacks.Load())
			}
		})
	}
}

func TestArchiveUnavailableRetryDelay(t *testing.T) {
	now := time.Date(2015, 10, 21, 7, 27, 51, 0, time.UTC)
	for _, tc := range []struct {
		raw   string
		delay time.Duration
		ok    bool
	}{{"7", 7 * time.Second, true}, {"Wed, 21 Oct 2015 07:28:00 GMT", 9 * time.Second, true}, {"invalid", 0, false}, {"", 0, false}, {"-5", 0, true}, {"9223372036854775807", 0, false}} {
		e := &ArchiveUnavailableError{RetryAfter: tc.raw}
		got, ok := e.RetryDelay(now)
		if got != tc.delay || ok != tc.ok {
			t.Errorf("%q: %v %v", tc.raw, got, ok)
		}
	}
}

func TestArchiveUnavailableDoesNotReclassifyOrigin503(t *testing.T) {
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FetchCache-Fetched-At", "2026-11-06T00:00:00Z")
		w.WriteHeader(503)
	}))
	defer cache.Close()
	c := NewClient(WithCacheURL(cache.URL), WithSource(SourceCommonCrawl))
	r, err := c.Get(context.Background(), "https://example.com/")
	if err != nil || r.Status != 503 {
		t.Fatalf("response=%+v err=%v", r, err)
	}
}

func TestArchiveOther5xxRemainsGeneric(t *testing.T) {
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer cache.Close()
	c := NewClient(WithCacheURL(cache.URL), WithSource(SourceCommonCrawl))
	_, err := c.Get(context.Background(), "https://example.com/")
	if err == nil || errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("unexpected classification: %v", err)
	}
}

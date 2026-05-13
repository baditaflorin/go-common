package apikey

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVerifier lets tests control the upstream response without
// standing up an http server.
type fakeVerifier struct {
	calls   int64
	respond func() (*VerifyResult, error)
}

func (f *fakeVerifier) Verify(_ context.Context, _ string) (*VerifyResult, error) {
	atomic.AddInt64(&f.calls, 1)
	return f.respond()
}

func TestCache_FreshTTL_SkipsUpstream(t *testing.T) {
	f := &fakeVerifier{respond: func() (*VerifyResult, error) {
		return &VerifyResult{User: "u", Scope: "*"}, nil
	}}
	c := NewCache(f)
	c.FreshTTL = time.Hour
	ctx := context.Background()

	// first call goes upstream, populates cache.
	if _, err := c.Verify(ctx, "k1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := atomic.LoadInt64(&f.calls); got != 1 {
		t.Errorf("first call should hit upstream once, got %d", got)
	}

	// next 10 calls within FreshTTL hit cache only.
	for i := 0; i < 10; i++ {
		if _, err := c.Verify(ctx, "k1"); err != nil {
			t.Fatalf("cached call: %v", err)
		}
	}
	if got := atomic.LoadInt64(&f.calls); got != 1 {
		t.Errorf("cached calls should stay at 1 upstream hit, got %d", got)
	}
}

func TestCache_InvalidKey_DropsCacheAndPropagates(t *testing.T) {
	state := int32(0) // 0 = return OK, 1 = return ErrInvalidKey
	f := &fakeVerifier{respond: func() (*VerifyResult, error) {
		if atomic.LoadInt32(&state) == 1 {
			return nil, ErrInvalidKey
		}
		return &VerifyResult{User: "u"}, nil
	}}
	c := NewCache(f)
	c.FreshTTL = time.Millisecond // force re-verify
	ctx := context.Background()

	if _, err := c.Verify(ctx, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	atomic.StoreInt32(&state, 1)
	_, err := c.Verify(ctx, "k")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
	// cache should be cleared — a third call still hits upstream.
	beforeCalls := atomic.LoadInt64(&f.calls)
	_, _ = c.Verify(ctx, "k")
	if atomic.LoadInt64(&f.calls) <= beforeCalls {
		t.Error("ErrInvalidKey should drop the cached entry so next call retries upstream")
	}
}

func TestCache_Unavailable_FallsBackToStaleEntry(t *testing.T) {
	state := int32(0)
	f := &fakeVerifier{respond: func() (*VerifyResult, error) {
		if atomic.LoadInt32(&state) == 1 {
			return nil, ErrKeystoreUnavailable
		}
		return &VerifyResult{User: "u", Scope: "s"}, nil
	}}
	c := NewCache(f)
	c.FreshTTL = time.Millisecond
	c.StaleTTL = time.Hour
	ctx := context.Background()

	if _, err := c.Verify(ctx, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	atomic.StoreInt32(&state, 1)
	res, err := c.Verify(ctx, "k")
	if err != nil {
		t.Fatalf("during outage, expected stale cache hit, got err: %v", err)
	}
	if res.User != "u" || res.Scope != "s" {
		t.Errorf("stale cache returned wrong values: %+v", res)
	}
}

func TestCache_Unavailable_PropagatesWhenNoStaleEntry(t *testing.T) {
	f := &fakeVerifier{respond: func() (*VerifyResult, error) {
		return nil, ErrKeystoreUnavailable
	}}
	c := NewCache(f)
	_, err := c.Verify(context.Background(), "k")
	if !errors.Is(err, ErrKeystoreUnavailable) {
		t.Fatalf("want ErrKeystoreUnavailable, got %v", err)
	}
}

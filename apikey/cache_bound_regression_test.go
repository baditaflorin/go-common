package apikey

import (
	"context"
	"fmt"
	"github.com/baditaflorin/go-common/clock"
	"testing"
	"time"
)

func TestPositiveCacheHasDefaultCapacity(t *testing.T) {
	f := &fakeVerifier{respond: func() (*VerifyResult, error) { return &VerifyResult{User: "test"}, nil }}
	c := NewCache(f)
	for i := range 4097 {
		if _, err := c.Verify(context.Background(), fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(c.Snapshot()); got > 4096 {
		t.Fatalf("positive cache retained %d entries, maximum 4096", got)
	}
}

func TestPositiveCacheUsesConfiguredCapacityAndWindows(t *testing.T) {
	mc := clock.NewMock(time.Unix(0, 0))
	f := &fakeVerifier{respond: func() (*VerifyResult, error) { return &VerifyResult{User: "test"}, nil }}
	c := NewCache(f)
	c.MaxEntries = 2
	c.FreshTTL = 2 * time.Hour
	c.StaleTTL = time.Hour
	c.clk = mc
	for _, key := range []string{"a", "b", "a", "c"} {
		if _, err := c.Verify(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := c.Snapshot()
	if len(snapshot) != 2 || snapshot["a"].IsZero() == true {
		t.Fatalf("snapshot=%v", snapshot)
	}
	if _, ok := snapshot["b"]; ok {
		t.Fatal("least recently used result was retained")
	}
	f.respond = func() (*VerifyResult, error) { return nil, ErrKeystoreUnavailable }
	mc.Advance(90 * time.Minute)
	if _, err := c.Verify(context.Background(), "a"); err != nil {
		t.Fatalf("FreshTTL greater than StaleTTL was not preserved: %v", err)
	}
	mc.Advance(30 * time.Minute)
	if got := len(c.Snapshot()); got != 0 {
		t.Fatalf("expired cache retained %d entries", got)
	}
}

func BenchmarkPositiveCacheHit(b *testing.B) {
	f := &fakeVerifier{respond: func() (*VerifyResult, error) { return &VerifyResult{User: "test"}, nil }}
	c := NewCache(f)
	c.MaxEntries = 128
	_, _ = c.Verify(context.Background(), "key")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = c.Verify(context.Background(), "key")
	}
}

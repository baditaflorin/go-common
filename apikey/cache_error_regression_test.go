package apikey

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCacheServesStaleOnlyForKeystoreOutage(t *testing.T) {
	unexpected := errors.New("verifier protocol failure")
	for _, tc := range []struct {
		name     string
		err      error
		canceled bool
		stale    bool
	}{
		{name: "outage", err: ErrKeystoreUnavailable, stale: true},
		{name: "wrapped outage", err: fmt.Errorf("node: %w", ErrKeystoreUnavailable), stale: true},
		{name: "unexpected", err: unexpected},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled outage", err: errors.Join(ErrKeystoreUnavailable, context.Canceled)},
		{name: "expired outage", err: errors.Join(ErrKeystoreUnavailable, context.DeadlineExceeded)},
		{name: "caller canceled", err: ErrKeystoreUnavailable, canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeVerifier{respond: func() (*VerifyResult, error) { return &VerifyResult{User: "u"}, nil }}
			c := NewCache(f)
			c.FreshTTL = -time.Second
			c.StaleTTL = time.Hour
			if _, err := c.Verify(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			f.respond = func() (*VerifyResult, error) { return nil, tc.err }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			result, err := c.Verify(ctx, "test")
			if tc.stale {
				if err != nil || result == nil || result.User != "u" {
					t.Fatalf("stale result=%v err=%v", result, err)
				}
				return
			}
			if result != nil || !errors.Is(err, tc.err) {
				t.Fatalf("result=%v err=%v, want underlying error %v", result, err, tc.err)
			}
		})
	}
}

package safehttp

import (
	"context"
	"errors"
	"testing"
)

func TestAmbiguousNumericIPv4BlockedBeforeResolver(t *testing.T) {
	for _, host := range []string{"0177.0.0.1", "0177.0.0.1.", "127.1", "2130706433", "0x7f000001", "0x7f.0.0.1", "127.000.0.1", "0300.0250.01.01", "0x8.8.8.8"} {
		for _, ctx := range []context.Context{context.Background(), RequireFetchCacheContext(context.Background())} {
			if err := GuardHost(ctx, host); !errors.Is(err, ErrBlocked) {
				t.Errorf("GuardHost(%q)=%v", host, err)
			}
			if _, err := CheckURL(ctx, "http://"+host+"/"); !errors.Is(err, ErrBlocked) {
				t.Errorf("CheckURL(%q)=%v", host, err)
			}
		}
	}
}
func TestAmbiguousNumericIPv4DoesNotFlagNormalHostnamesOrCanonicalIPs(t *testing.T) {
	for _, host := range []string{"8.8.8.8", "8.8.8.8.", "1.1.1.1", "123.example.com", "foo-123.example", "2001:4860:4860::8888"} {
		if isAmbiguousNumericIPv4(host) {
			t.Errorf("unexpected ambiguous host %q", host)
		}
	}
}

package safehttp_test

import (
	"context"
	"net"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

func BenchmarkIsBlocked(b *testing.B) {
	ip := net.ParseIP("93.184.216.34")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = safehttp.IsBlocked(ip)
	}
}

func BenchmarkNormalizeURL(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := safehttp.NormalizeURL("  Example.com/path?q=1 "); err != nil {
			b.Fatal(err)
		}
	}
}

// A literal public IP is judged without the resolver, so this measures the guard
// itself.
func BenchmarkGuardHostLiteralIP(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := safehttp.GuardHost(ctx, "93.184.216.34"); err != nil {
			b.Fatal(err)
		}
	}
}

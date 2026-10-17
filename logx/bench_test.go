package logx

import (
	"context"
	"io"
	"testing"
)

func BenchmarkLogRecord(b *testing.B) {
	l := New(Options{Service: "bench", Version: "1", Output: io.Discard})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "request_completed", "status", 200, "path", "/v1/items", "duration", "1.2ms")
	}
}

func BenchmarkRedactStringClean(b *testing.B) {
	s := "request_completed status=200 path=/v1/items duration=1.2ms upstream=ledger"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactString(s)
	}
}

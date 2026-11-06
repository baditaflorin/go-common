package metrics

import (
	"strings"
	"testing"
)

func BenchmarkMetricsRecordPath(b *testing.B) {
	for _, path := range []string{"/work", strings.Repeat("p", 4096)} {
		name := "Warm"
		if len(path) > 100 {
			name = "Oversized"
		}
		b.Run(name, func(b *testing.B) {
			s := New()
			s.Record(200, 0, path)
			b.ReportAllocs()
			for b.Loop() {
				s.Record(200, 0, path)
			}
		})
	}
}

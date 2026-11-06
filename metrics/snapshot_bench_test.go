package metrics

import (
	"testing"
	"time"
)

var snapshotSink Stats

func BenchmarkMetricsSnapshotFullWindow(b *testing.B) {
	s := New()
	for i := 0; i < MaxLatencySamples; i++ {
		s.Record(200, time.Duration((i*7919)%MaxLatencySamples)*time.Microsecond, "/work")
	}
	b.ReportAllocs()
	for b.Loop() {
		snapshotSink = s.Snapshot()
	}
}

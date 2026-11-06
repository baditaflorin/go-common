package safehttp

import (
	"fmt"
	"testing"
	"time"
)

func TestFailureTrackingRequiresConsumer(t *testing.T) {
	tr := &extrasTransport{}
	tr.recordFailure("example.com", 503, 0)
	if len(tr.hostState) != 0 {
		t.Fatal("unused failure tracking retained host state")
	}
}

func TestFailureTrackingHasCapacity(t *testing.T) {
	tr := &extrasTransport{backoffURL: "https://coordinator.example"}
	for i := range 8193 {
		tr.recordFailure(fmt.Sprintf("host%d.example.com", i), 503, 0)
	}
	if got := len(tr.hostState); got > 8192 {
		t.Fatalf("retained %d hosts, maximum8192", got)
	}
}

func TestFailureTrackingPrunesUntouchedExpiredHosts(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &extrasTransport{backoffURL: "https://coordinator.example", hostNow: func() time.Time { return now }}
	tr.recordFailure("old.example.com", 503, 0)
	now = now.Add(hostFailureTTL + time.Nanosecond)
	tr.recordFailure("new.example.com", 429, 7)
	if _, ok := tr.hostState["old.example.com"]; ok {
		t.Fatal("untouched expired host remained resident")
	}
	if f, ok := tr.recentFailure("new.example.com"); !ok || f.retryAfterSeconds != 7 {
		t.Fatalf("new failure=%v present=%v", f, ok)
	}
	tr.clearFailure("new.example.com")
	if _, ok := tr.recentFailure("new.example.com"); ok {
		t.Fatal("success did not clear failure")
	}
}

func BenchmarkUnusedFailureTracking(b *testing.B) {
	tr := &extrasTransport{}
	b.ReportAllocs()
	for range b.N {
		tr.recordFailure("example.com", 503, 0)
	}
}

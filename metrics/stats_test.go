package metrics

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRecordAndSnapshotBasics(t *testing.T) {
	s := New()
	s.Record(200, 5*time.Millisecond, "/a")
	s.Record(200, 50*time.Millisecond, "/a")
	s.Record(404, 5*time.Millisecond, "/b")
	s.Record(500, 2*time.Second, "/c")

	snap := s.Snapshot()
	if snap.TotalRequests != 4 {
		t.Errorf("TotalRequests = %d, want 4", snap.TotalRequests)
	}
	if snap.TotalErrors != 2 { // 404 + 500
		t.Errorf("TotalErrors = %d, want 2", snap.TotalErrors)
	}
	if snap.StatusCounts[200] != 2 || snap.StatusCounts[404] != 1 || snap.StatusCounts[500] != 1 {
		t.Errorf("StatusCounts = %v", snap.StatusCounts)
	}
	if snap.PathStats["/a"] != 2 || snap.PathStats["/b"] != 1 || snap.PathStats["/c"] != 1 {
		t.Errorf("PathStats = %v", snap.PathStats)
	}
	if snap.Latency.Buckets["<10ms"] != 2 { // two 5ms
		t.Errorf("bucket <10ms = %d, want 2", snap.Latency.Buckets["<10ms"])
	}
	if snap.Latency.Buckets[">1s"] != 1 { // the 2s
		t.Errorf("bucket >1s = %d, want 1", snap.Latency.Buckets[">1s"])
	}
	if snap.Latency.P50 == "n/a" {
		t.Errorf("expected percentiles computed, got n/a")
	}
	if snap.LastRequest.IsZero() {
		t.Errorf("LastRequest should be set")
	}
}

func TestPathStatsCardinalityCap(t *testing.T) {
	s := New()
	// Far more than maxTrackedPaths distinct paths.
	for i := 0; i < maxTrackedPaths+500; i++ {
		s.Record(200, time.Millisecond, "/p/"+strconv.Itoa(i))
	}
	snap := s.Snapshot()
	// Distinct keys must be bounded: at most maxTrackedPaths admitted + "_other".
	if len(snap.PathStats) > maxTrackedPaths+1 {
		t.Errorf("PathStats has %d keys, want <= %d", len(snap.PathStats), maxTrackedPaths+1)
	}
	if snap.PathStats["_other"] == 0 {
		t.Errorf("expected overflow paths folded into _other")
	}
	// Total across all path counters must still equal request count.
	var sum int64
	for _, v := range snap.PathStats {
		sum += v
	}
	if sum != snap.TotalRequests {
		t.Errorf("path counts sum %d != total requests %d", sum, snap.TotalRequests)
	}
}

func TestSnapshotIsReadOnlyCopy(t *testing.T) {
	s := New()
	s.Record(200, time.Millisecond, "/x")
	snap := s.Snapshot()
	// Recording into a snapshot copy must be a no-op (live == nil).
	snap.Record(500, time.Second, "/y")
	if snap.TotalRequests != 1 {
		t.Errorf("snapshot mutated: TotalRequests = %d, want 1", snap.TotalRequests)
	}
}

func TestRecordConcurrent(t *testing.T) {
	s := New()
	const goroutines, per = 16, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				s.Record(200, time.Millisecond, "/c")
			}
		}()
	}
	wg.Wait()
	snap := s.Snapshot()
	if want := int64(goroutines * per); snap.TotalRequests != want {
		t.Errorf("TotalRequests = %d, want %d", snap.TotalRequests, want)
	}
	if snap.StatusCounts[200] != int64(goroutines*per) {
		t.Errorf("StatusCounts[200] = %d, want %d", snap.StatusCounts[200], goroutines*per)
	}
}

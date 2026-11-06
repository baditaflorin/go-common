package metrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestPathAdmissionIsBoundedUnderConcurrency(t *testing.T) {
	s := New()
	for i := 0; i < maxTrackedPaths-1; i++ {
		s.Record(200, time.Millisecond, fmt.Sprintf("/prefill/%d", i))
	}
	const workers = 256
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; s.Record(200, time.Millisecond, fmt.Sprintf("/parallel/%d", i)) }(i)
	}
	close(start)
	wg.Wait()
	snapshot := s.Snapshot()
	if len(snapshot.PathStats) > maxTrackedPaths+1 {
		t.Fatalf("retained %d paths", len(snapshot.PathStats))
	}
	var total int64
	for _, n := range snapshot.PathStats {
		total += n
	}
	if total != maxTrackedPaths-1+workers || total != snapshot.TotalRequests {
		t.Fatalf("lost counts: paths=%d requests=%d", total, snapshot.TotalRequests)
	}
}

func TestPathRetentionBoundsBytesAndOwnsBacking(t *testing.T) {
	s := New()
	s.Record(200, 0, strings.Repeat("p", maxTrackedPathBytes+1))
	s.Record(200, 0, "/bad/\xff")
	backing := "/short" + strings.Repeat("q", 1<<20)
	short := backing[:len("/short")]
	s.Record(200, 0, short)
	s.live.pathStats.Range(func(key, _ any) bool {
		path := key.(string)
		if len(path) > maxTrackedPathBytes {
			t.Errorf("retained path has %d bytes", len(path))
		}
		if path == short && unsafe.StringData(path) == unsafe.StringData(short) {
			t.Error("short key retains oversized request backing")
		}
		return true
	})
	if v, ok := s.live.pathStats.Load("_other"); !ok || v == nil {
		t.Fatal("oversized/invalid paths were not folded")
	}
	if got := s.Snapshot().PathStats["_other"]; got != 2 {
		t.Fatalf("overflow count = %d", got)
	}
}

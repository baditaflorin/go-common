package metrics

import (
	"runtime"
	"sync"
	"time"
)

type Stats struct {
	mu            sync.RWMutex
	TotalRequests int64            `json:"total_requests"`
	TotalErrors   int64            `json:"total_errors"`
	StartTime     time.Time        `json:"start_time"`
	Uptime        string           `json:"uptime"`
	LastRequest   time.Time        `json:"last_request"`
	StatusCounts  map[int]int64    `json:"status_counts"`
	Latency       LatencyStats     `json:"latency"`
	System        SystemStats      `json:"system"`
	PathStats     map[string]int64 `json:"path_stats"` // Simple path counter
}

type LatencyStats struct {
	TotalDuration time.Duration    `json:"-"`
	AvgDuration   string           `json:"avg_duration"`
	Buckets       map[string]int64 `json:"buckets"` // <100ms, <500ms, <1s, >1s
}

type SystemStats struct {
	Goroutines int    `json:"goroutines"`
	HeapAlloc  string `json:"heap_alloc"`
	StackInUse string `json:"stack_in_use"`
}

func New() *Stats {
	return &Stats{
		StartTime:    time.Now(),
		StatusCounts: make(map[int]int64),
		PathStats:    make(map[string]int64),
		Latency: LatencyStats{
			Buckets: map[string]int64{
				"<10ms":  0,
				"<100ms": 0,
				"<500ms": 0,
				"<1s":    0,
				">1s":    0,
			},
		},
	}
}

func (s *Stats) Record(statusCode int, duration time.Duration, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalRequests++
	if statusCode >= 400 {
		s.TotalErrors++
	}
	s.StatusCounts[statusCode]++
	s.LastRequest = time.Now()

	// Path Tracking (limit to prevent explosion? - simplistic for now)
	if len(s.PathStats) < 1000 {
		s.PathStats[path]++
	}

	// Latency
	s.Latency.TotalDuration += duration
	if duration < 10*time.Millisecond {
		s.Latency.Buckets["<10ms"]++
	} else if duration < 100*time.Millisecond {
		s.Latency.Buckets["<100ms"]++
	} else if duration < 500*time.Millisecond {
		s.Latency.Buckets["<500ms"]++
	} else if duration < 1*time.Second {
		s.Latency.Buckets["<1s"]++
	} else {
		s.Latency.Buckets[">1s"]++
	}
}

func (s *Stats) Snapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Copy maps
	counts := make(map[int]int64, len(s.StatusCounts))
	for k, v := range s.StatusCounts {
		counts[k] = v
	}

	buckets := make(map[string]int64, len(s.Latency.Buckets))
	for k, v := range s.Latency.Buckets {
		buckets[k] = v
	}

	paths := make(map[string]int64, len(s.PathStats))
	for k, v := range s.PathStats {
		paths[k] = v
	}

	// Calculate Avg
	avg := "0ms"
	if s.TotalRequests > 0 {
		avg = (s.Latency.TotalDuration / time.Duration(s.TotalRequests)).String()
	}

	// Runtime Stats
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return Stats{
		TotalRequests: s.TotalRequests,
		TotalErrors:   s.TotalErrors,
		StartTime:     s.StartTime,
		Uptime:        time.Since(s.StartTime).String(),
		LastRequest:   s.LastRequest,
		StatusCounts:  counts,
		PathStats:     paths,
		Latency: LatencyStats{
			AvgDuration: avg,
			Buckets:     buckets,
		},
		System: SystemStats{
			Goroutines: runtime.NumGoroutine(),
			HeapAlloc:  byteCountDecimal(mem.HeapAlloc),
			StackInUse: byteCountDecimal(mem.StackInuse),
		},
	}
}

func byteCountDecimal(b uint64) string {
	const unit = 1000
	if b < unit {
		return string(b) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return string(b/uint64(div)) + string("kMGTPE"[exp]) + "B" // Simplified
}

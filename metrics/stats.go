package metrics

import (
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

// MaxLatencySamples is the ring-buffer cap for raw latency samples
// used to compute percentiles. Older samples are dropped once full.
const MaxLatencySamples = 10_000

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
	P50           string           `json:"p50"`     // median latency
	P95           string           `json:"p95"`     // 95th percentile
	P99           string           `json:"p99"`     // 99th percentile
	Buckets       map[string]int64 `json:"buckets"` // <10ms, <100ms, <500ms, <1s, >1s

	// rawSamples stores the last MaxLatencySamples durations for
	// accurate percentile computation. Ring-buffer capped at 10k entries.
	rawSamples []time.Duration
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

	// Latency buckets.
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

	// Raw sample ring-buffer for percentile computation.
	if len(s.Latency.rawSamples) < MaxLatencySamples {
		s.Latency.rawSamples = append(s.Latency.rawSamples, duration)
	} else {
		// Replace oldest entry (simple ring via modulo on total requests).
		idx := int(s.TotalRequests-1) % MaxLatencySamples
		s.Latency.rawSamples[idx] = duration
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

	// Copy raw samples for percentile computation (done outside the lock).
	rawCopy := make([]time.Duration, len(s.Latency.rawSamples))
	copy(rawCopy, s.Latency.rawSamples)

	// Calculate Avg
	avg := "0ms"
	if s.TotalRequests > 0 {
		avg = (s.Latency.TotalDuration / time.Duration(s.TotalRequests)).String()
	}

	// Runtime Stats
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Compute percentiles from the sample copy (outside lock).
	p50, p95, p99 := computePercentiles(rawCopy)

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
			P50:         p50,
			P95:         p95,
			P99:         p99,
			Buckets:     buckets,
		},
		System: SystemStats{
			Goroutines: runtime.NumGoroutine(),
			HeapAlloc:  byteCountDecimal(mem.HeapAlloc),
			StackInUse: byteCountDecimal(mem.StackInuse),
		},
	}
}

// computePercentiles returns the p50, p95, and p99 latency strings
// from a slice of raw samples. Returns "n/a" when no samples exist.
func computePercentiles(samples []time.Duration) (p50, p95, p99 string) {
	if len(samples) == 0 {
		return "n/a", "n/a", "n/a"
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[percentileIdx(len(sorted), 50)].String(),
		sorted[percentileIdx(len(sorted), 95)].String(),
		sorted[percentileIdx(len(sorted), 99)].String()
}

// percentileIdx returns the 0-based index for the given percentile p
// in a sorted slice of length n.
func percentileIdx(n, p int) int {
	idx := int(float64(n)*float64(p)/100.0) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

func byteCountDecimal(b uint64) string {
	const unit = 1000
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatUint(b/div, 10) + string("kMGTPE"[exp]) + "B"
}

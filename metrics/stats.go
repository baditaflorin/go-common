package metrics

import (
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// MaxLatencySamples is the ring-buffer cap for raw latency samples
// used to compute percentiles. Older samples are dropped once full.
const MaxLatencySamples = 10_000

// maxTrackedPaths bounds how many distinct paths PathStats retains, so a
// parameterised path or scanner traffic can't grow the map without limit
// (same cardinality hazard fixed for the promx route label). Distinct
// paths beyond the cap are counted under the "_other" key.
const maxTrackedPaths = 1000

// Stats is the request-metrics accumulator. Its exported fields carry the
// JSON shape served at /stats; on a live accumulator (from New) they stay
// zero — the running totals live in the lock-free `live` accumulator and are
// materialised onto the exported fields only by Snapshot. This keeps the
// per-request Record path allocation- and lock-free while preserving the
// exact public type + JSON contract.
type Stats struct {
	TotalRequests int64            `json:"total_requests"`
	TotalErrors   int64            `json:"total_errors"`
	StartTime     time.Time        `json:"start_time"`
	Uptime        string           `json:"uptime"`
	LastRequest   time.Time        `json:"last_request"`
	StatusCounts  map[int]int64    `json:"status_counts"`
	Latency       LatencyStats     `json:"latency"`
	System        SystemStats      `json:"system"`
	PathStats     map[string]int64 `json:"path_stats"` // Simple path counter

	live *live // nil on a Snapshot copy; non-nil on a live New() accumulator
}

type LatencyStats struct {
	TotalDuration time.Duration    `json:"-"`
	AvgDuration   string           `json:"avg_duration"`
	P50           string           `json:"p50"`     // median latency
	P95           string           `json:"p95"`     // 95th percentile
	P99           string           `json:"p99"`     // 99th percentile
	Buckets       map[string]int64 `json:"buckets"` // <10ms, <100ms, <500ms, <1s, >1s
}

type SystemStats struct {
	Goroutines int    `json:"goroutines"`
	HeapAlloc  string `json:"heap_alloc"`
	StackInUse string `json:"stack_in_use"`
}

// bucketLabels maps the fixed latency bucket index to its JSON key. The
// order matches the < thresholds checked in live.record.
var bucketLabels = [5]string{"<10ms", "<100ms", "<500ms", "<1s", ">1s"}

// live is the lock-free accumulator behind a Stats. Every field is updated
// with atomics on the request hot path — no mutex, no map write — so the
// per-request cost is a handful of atomic adds. PathStats (a genuine map)
// is the one structure needing coordination; it uses sync.Map with atomic
// counters, bounded by maxTrackedPaths.
type live struct {
	totalRequests atomic.Int64
	totalErrors   atomic.Int64
	totalDurNanos atomic.Int64
	lastReqNanos  atomic.Int64

	// statusCounts indexed by HTTP status code (0..599). Codes outside the
	// range fold to index 0. Lock-free, fixed footprint.
	statusCounts [600]atomic.Int64

	buckets [5]atomic.Int64

	// rawSamples is a fixed-size ring of latency nanos for percentiles;
	// sampleSeq selects the slot. Pre-allocated so the hot path never grows
	// a slice or takes a lock.
	rawSamples []atomic.Int64
	sampleSeq  atomic.Int64

	pathStats sync.Map // string -> *atomic.Int64
	pathLen   atomic.Int64
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
		live: &live{rawSamples: make([]atomic.Int64, MaxLatencySamples)},
	}
}

func (s *Stats) Record(statusCode int, duration time.Duration, path string) {
	l := s.live
	if l == nil { // a Snapshot copy is read-only; never recorded into
		return
	}

	l.totalRequests.Add(1)
	if statusCode >= 400 {
		l.totalErrors.Add(1)
	}
	if statusCode >= 0 && statusCode < len(l.statusCounts) {
		l.statusCounts[statusCode].Add(1)
	} else {
		l.statusCounts[0].Add(1)
	}
	l.lastReqNanos.Store(time.Now().UnixNano())

	l.recordPath(path)

	l.totalDurNanos.Add(int64(duration))
	l.buckets[bucketIndex(duration)].Add(1)

	// Ring-buffer the raw sample for percentile computation (lock-free).
	seq := l.sampleSeq.Add(1)
	l.rawSamples[(seq-1)%MaxLatencySamples].Store(int64(duration))
}

// recordPath increments the per-path counter, bounding distinct paths to
// maxTrackedPaths (overflow folds to "_other") so the map can't grow without
// limit under parameterised paths or scanner traffic.
func (l *live) recordPath(path string) {
	if v, ok := l.pathStats.Load(path); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	if l.pathLen.Load() >= maxTrackedPaths {
		path = "_other"
		if v, ok := l.pathStats.Load(path); ok {
			v.(*atomic.Int64).Add(1)
			return
		}
	}
	ctr := new(atomic.Int64)
	ctr.Add(1)
	if actual, loaded := l.pathStats.LoadOrStore(path, ctr); loaded {
		actual.(*atomic.Int64).Add(1)
	} else {
		l.pathLen.Add(1)
	}
}

func bucketIndex(d time.Duration) int {
	switch {
	case d < 10*time.Millisecond:
		return 0
	case d < 100*time.Millisecond:
		return 1
	case d < 500*time.Millisecond:
		return 2
	case d < time.Second:
		return 3
	default:
		return 4
	}
}

func (s *Stats) Snapshot() Stats {
	l := s.live
	if l == nil {
		return *s // already a snapshot
	}

	total := l.totalRequests.Load()

	counts := make(map[int]int64)
	for code := range l.statusCounts {
		if v := l.statusCounts[code].Load(); v > 0 {
			counts[code] = v
		}
	}

	buckets := make(map[string]int64, len(bucketLabels))
	for i, label := range bucketLabels {
		buckets[label] = l.buckets[i].Load()
	}

	paths := make(map[string]int64)
	l.pathStats.Range(func(k, v any) bool {
		paths[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})

	// Copy the populated portion of the ring for percentile computation.
	n := int(l.sampleSeq.Load())
	if n > MaxLatencySamples {
		n = MaxLatencySamples
	}
	rawCopy := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		rawCopy[i] = time.Duration(l.rawSamples[i].Load())
	}

	totalDur := time.Duration(l.totalDurNanos.Load())
	avg := "0ms"
	if total > 0 {
		avg = (totalDur / time.Duration(total)).String()
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	p50, p95, p99 := computePercentiles(rawCopy)

	var lastReq time.Time
	if ns := l.lastReqNanos.Load(); ns > 0 {
		lastReq = time.Unix(0, ns)
	}

	return Stats{
		TotalRequests: total,
		TotalErrors:   l.totalErrors.Load(),
		StartTime:     s.StartTime,
		Uptime:        time.Since(s.StartTime).String(),
		LastRequest:   lastReq,
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

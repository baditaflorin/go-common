package metrics

import (
	"sync"
	"time"
)

type Stats struct {
	mu            sync.RWMutex
	TotalRequests int64         `json:"total_requests"`
	TotalErrors   int64         `json:"total_errors"`
	StartTime     time.Time     `json:"start_time"`
	StatusCounts  map[int]int64 `json:"status_counts"`
}

func New() *Stats {
	return &Stats{
		StartTime:    time.Now(),
		StatusCounts: make(map[int]int64),
	}
}

func (s *Stats) Record(statusCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalRequests++
	if statusCode >= 400 {
		s.TotalErrors++
	}
	s.StatusCounts[statusCode]++
}

func (s *Stats) Snapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Copy map
	counts := make(map[int]int64)
	for k, v := range s.StatusCounts {
		counts[k] = v
	}

	return Stats{
		TotalRequests: s.TotalRequests,
		TotalErrors:   s.TotalErrors,
		StartTime:     s.StartTime,
		StatusCounts:  counts,
	}
}

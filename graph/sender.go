package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// sender runs a background goroutine that periodically drains the ring
// and POSTs batches to the collector. Owns its own *http.Client — we
// cannot use safehttp here because it would import graph itself.
type sender struct {
	cfg       config
	serviceID string
	version   string
	ring      *ring
	counters  *atomicCounters
	client    *http.Client
	stop      chan struct{}
	stopped   chan struct{}
}

func newSender(cfg config, serviceID, version string, r *ring, c *atomicCounters) *sender {
	return &sender{
		cfg:       cfg,
		serviceID: serviceID,
		version:   version,
		ring:      r,
		counters:  c,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (s *sender) run() {
	defer close(s.stopped)
	if !s.cfg.enabled || s.cfg.collectorURL == "" {
		// Disabled or no collector — still drain on shutdown signal
		// so memory doesn't grow unbounded if Init runs without a URL.
		<-s.stop
		return
	}
	tick := time.NewTicker(s.cfg.flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			s.flush() // best-effort final flush
			return
		case <-tick.C:
			s.flush()
		}
	}
}

func (s *sender) flush() {
	for {
		events := s.ring.drain(s.cfg.flushBatch)
		if len(events) == 0 {
			return
		}
		s.send(events)
		if len(events) < s.cfg.flushBatch {
			return // ring is now empty
		}
	}
}

func (s *sender) send(events []Event) {
	batch := Batch{
		Service:       s.serviceID,
		Version:       s.version,
		SchemaVersion: SchemaVersion,
		Events:        events,
	}
	body, err := json.Marshal(batch)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.collectorURL+"/events", bytes.NewReader(body))
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "go-common-graph/"+s.version+" ("+s.serviceID+")")
	if s.cfg.apiKey != "" {
		req.Header.Set("X-API-Key", s.cfg.apiKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		atomic.AddInt64(&s.counters.BatchesSent, 1)
	} else {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
	}
}

func (s *sender) shutdown() {
	close(s.stop)
	<-s.stopped
}

// atomicCounters holds Counters with atomic accessors. We keep the
// public Counters as a plain struct (good for JSON) and copy out
// under Stats().
type atomicCounters struct {
	EventsRecorded int64
	EventsDropped  int64
	EventsSampled  int64
	BatchesSent    int64
	BatchesFailed  int64
}

func (c *atomicCounters) snapshot() Counters {
	return Counters{
		EventsRecorded: atomic.LoadInt64(&c.EventsRecorded),
		EventsDropped:  atomic.LoadInt64(&c.EventsDropped),
		EventsSampled:  atomic.LoadInt64(&c.EventsSampled),
		BatchesSent:    atomic.LoadInt64(&c.BatchesSent),
		BatchesFailed:  atomic.LoadInt64(&c.BatchesFailed),
	}
}

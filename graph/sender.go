package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/header"
)

const graphAuthFailureCooldown = time.Minute

// sender runs a background goroutine that periodically drains the ring
// and POSTs batches to the collector. Owns its own *http.Client — we
// cannot use safehttp here because it would import graph itself.
type sender struct {
	cfg       config
	serviceID string
	version   string
	ring      *ring
	// pending holds at most one failed batch outside the ring. This
	// preserves the batch for a later retry without draining additional
	// observations during the same failing flush.
	pending           []Event
	counters          *atomicCounters
	client            *http.Client
	stop              chan struct{}
	stopped           chan struct{}
	authCooldownUntil int64 // unix nanos; accessed atomically
}

func newSender(cfg config, serviceID, version string, r *ring, c *atomicCounters) *sender {
	return &sender{
		cfg:       cfg,
		serviceID: serviceID,
		version:   version,
		ring:      r,
		counters:  c,
		client:    newGraphHTTPClient(5 * time.Second),
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

func (s *sender) run() {
	defer close(s.stopped)
	if !s.cfg.eventEmissionEnabled() {
		// Disabled, misconfigured, or missing a writer key. Record is a
		// no-op in this state and this process makes no request.
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
	if !s.canWriteAt(time.Now()) {
		return
	}
	for {
		events := s.pending
		if len(events) == 0 {
			events = s.ring.drain(s.cfg.flushBatch)
		}
		if len(events) == 0 {
			return
		}
		if !s.send(events) {
			// One failed batch must not turn into a burst that drains the
			// remainder of the ring. Preserve the failed batch separately
			// and leave every later batch queued in the ring for a later tick.
			s.pending = events
			return
		}
		s.pending = nil
		if len(events) < s.cfg.flushBatch {
			return // ring is now empty
		}
	}
}

// send returns true only when the collector accepted the batch. The result
// drives flush's fail-closed pacing: after one failure, no more batches leave
// this process until a later scheduled flush.
func (s *sender) send(events []Event) bool {
	if !s.canWriteAt(time.Now()) {
		return false
	}
	batch := Batch{
		Service:       s.serviceID,
		Version:       s.version,
		SchemaVersion: SchemaVersion,
		Events:        events,
	}
	body, err := json.Marshal(batch)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.collectorURL+"/events", bytes.NewReader(body))
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "go-common-graph/"+s.version+" ("+s.serviceID+")")
	req.Header.Set(header.APIKey, s.cfg.writerAPIKey)
	resp, err := s.client.Do(req)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return false
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		atomic.AddInt64(&s.counters.BatchesSent, 1)
		return true
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Auth failures indicate a policy/key mismatch, not transient
		// backpressure. Cool down for a minute to avoid hammering the
		// verifier while preserving the remaining ring for observation.
		atomic.StoreInt64(&s.authCooldownUntil, time.Now().Add(graphAuthFailureCooldown).UnixNano())
	}
	atomic.AddInt64(&s.counters.BatchesFailed, 1)
	return false
}

func (s *sender) configuredForWrite() bool {
	return s.cfg.eventEmissionEnabled()
}

func (s *sender) canWriteAt(now time.Time) bool {
	if !s.configuredForWrite() {
		return false
	}
	until := atomic.LoadInt64(&s.authCooldownUntil)
	return until == 0 || !now.Before(time.Unix(0, until))
}

// newGraphHTTPClient is shared by event writes and read-only lookups. It
// never follows redirects, keeping X-API-Key bound to the configured
// canonical endpoint instead of a Location header supplied by a server.
func newGraphHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		},
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

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/header"
	"golang.org/x/net/http/httpproxy"
)

const graphAuthFailureCooldown = time.Minute

// unknownIdentity is the placeholder older versions stamped on events recorded
// before Init; the collector rejects it as the writer's own identity.
const unknownIdentity = "unknown"

// sendResult is the collector's verdict on one batch.
type sendResult int

const (
	// sendRetain: not delivered, but the same batch may succeed later
	// (transport error, 5xx, 401 until the key is rotated, 408, 429, ...).
	sendRetain sendResult = iota
	sendOK
	// sendRejected: the collector refused this exact batch and will refuse it
	// again, so keeping it would block every later batch behind it.
	sendRejected
)

// rejectedStatus reports whether an HTTP status means the collector refused
// the batch's content: 403 (an event does not belong to the authenticated
// writer), or a malformed / oversized / unsupported / unprocessable payload.
// 401 is deliberately absent: an expired key can be rotated and the batch
// delivered afterwards.
func rejectedStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// sender runs a background goroutine that periodically drains the ring
// and POSTs batches to the collector. Owns its own *http.Client — we
// cannot use safehttp here because it would import graph itself.
type sender struct {
	cfg        config
	identityMu sync.RWMutex
	serviceID  string
	version    string
	ring       *ring
	// pending holds at most one failed batch outside the ring. This
	// preserves the batch for a later retry without draining additional
	// observations during the same failing flush.
	pending           []Event
	counters          *atomicCounters
	client            *http.Client
	stop              chan struct{}
	stopped           chan struct{}
	stopOnce          sync.Once
	shutdownCtx       context.Context
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

func (s *sender) setIdentity(serviceID, version string) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	s.serviceID = serviceID
	if version != "" {
		s.version = version
	}
}

func (s *sender) identity() (string, string) {
	s.identityMu.RLock()
	defer s.identityMu.RUnlock()
	return s.serviceID, s.version
}

func (s *sender) run() {
	defer close(s.stopped)
	tick := time.NewTicker(s.cfg.flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			s.flush(s.shutdownCtx) // bounded best-effort final flush
			return
		case <-tick.C:
			s.flush(context.Background())
		}
	}
}

func (s *sender) flush(contexts ...context.Context) {
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	if !s.canWriteAt(time.Now()) {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if id, _ := s.identity(); id == "" {
			// Identity not known yet (no Init): keep everything in the ring
			// rather than ship events under a placeholder the collector rejects.
			return
		}
		events := s.pending
		if len(events) == 0 {
			events = s.ring.drain(s.cfg.flushBatch)
		}
		if len(events) == 0 {
			return
		}
		switch s.send(ctx, events) {
		case sendOK:
			s.pending = nil
			if len(events) < s.cfg.flushBatch {
				return // ring is now empty
			}
		case sendRejected:
			// Retrying the same bytes can never succeed; keeping the batch in
			// pending would block every later batch until restart. Drop it, and
			// leave the next batch for the next tick like any other failure.
			s.pending = nil
			return
		default:
			// One failed batch must not turn into a burst that drains the
			// remainder of the ring. Preserve the failed batch separately
			// and leave every later batch queued in the ring for a later tick.
			s.pending = events
			return
		}
	}
}

// send reports the collector's verdict on the batch. After anything but
// sendOK, flush stops: no more batches leave this process until a later
// scheduled flush.
func (s *sender) send(parent context.Context, events []Event) sendResult {
	if !s.canWriteAt(time.Now()) {
		return sendRetain
	}
	writerAPIKey := s.cfg.currentWriterAPIKey()
	if writerAPIKey == "" {
		return sendRetain
	}
	serviceID, version := s.identity()
	if serviceID == "" {
		return sendRetain
	}
	stampOwnIdentity(events, serviceID)
	batch := Batch{
		Service:       serviceID,
		Version:       version,
		SchemaVersion: SchemaVersion,
		Events:        events,
	}
	body, err := json.Marshal(batch)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return sendRetain
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.collectorURL+"/events", bytes.NewReader(body))
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return sendRetain
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "go-common-graph/"+version+" ("+serviceID+")")
	req.Header.Set(header.APIKey, writerAPIKey)
	resp, err := s.client.Do(req)
	if err != nil {
		atomic.AddInt64(&s.counters.BatchesFailed, 1)
		return sendRetain
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		atomic.AddInt64(&s.counters.BatchesSent, 1)
		return sendOK
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Auth failures indicate a policy/key mismatch, not transient
		// backpressure. Cool down for a minute to avoid hammering the
		// verifier while preserving the remaining ring for observation.
		atomic.StoreInt64(&s.authCooldownUntil, time.Now().Add(graphAuthFailureCooldown).UnixNano())
	}
	atomic.AddInt64(&s.counters.BatchesFailed, 1)
	if rejectedStatus(resp.StatusCode) {
		atomic.AddInt64(&s.counters.BatchesRejected, 1)
		return sendRejected
	}
	return sendRetain
}

// stampOwnIdentity fills the service's own side of each event (the caller of
// an outbound call, the target of an inbound one) when the event was recorded
// before the identity was known. The other side is never touched: an inbound
// event's caller may legitimately be "unknown".
func stampOwnIdentity(events []Event, serviceID string) {
	for i := range events {
		e := &events[i]
		switch e.Direction {
		case "out":
			if e.Caller == "" || e.Caller == unknownIdentity {
				e.Caller = serviceID
			}
		case "in":
			if e.Target == "" || e.Target == unknownIdentity {
				e.Target = serviceID
			}
		}
	}
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
// routes the public graph collector through the configured egress proxy even
// when the fleet-wide NO_PROXY suffix also matches .0exec.com. The proxy
// package still keeps loopback-only development collectors direct. Redirects
// remain disabled so X-API-Key stays bound to the canonical endpoint.
func newGraphHTTPClient(timeout time.Duration) *http.Client {
	proxyConfig := httpproxy.FromEnvironment()
	proxyConfig.NoProxy = ""
	proxyForURL := proxyConfig.ProxyFunc()

	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: func(req *http.Request) (*url.URL, error) {
				return proxyForURL(req.URL)
			},
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}
}

func (s *sender) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.stopOnce.Do(func() {
		s.shutdownCtx = ctx
		close(s.stop)
	})
	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	// BatchesRejected counts batches the collector refused for their content
	// and that were dropped (a subset of BatchesFailed).
	BatchesRejected int64
}

func (c *atomicCounters) snapshot() Counters {
	return Counters{
		EventsRecorded:  atomic.LoadInt64(&c.EventsRecorded),
		EventsDropped:   atomic.LoadInt64(&c.EventsDropped),
		EventsSampled:   atomic.LoadInt64(&c.EventsSampled),
		BatchesSent:     atomic.LoadInt64(&c.BatchesSent),
		BatchesFailed:   atomic.LoadInt64(&c.BatchesFailed),
		BatchesRejected: atomic.LoadInt64(&c.BatchesRejected),
	}
}

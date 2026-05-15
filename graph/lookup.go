package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrNotConfigured is returned by Lookup when GRAPH_COLLECTOR_URL is
// unset. Callers fall back to a hardcoded URL in that case.
var ErrNotConfigured = errors.New("graph: collector URL not configured")

// ErrNotFound is returned when the collector has no entry for the slug.
var ErrNotFound = errors.New("graph: service not found")

// lookupCache memoises positive results for 5 minutes. The collector
// is the source of truth and rarely changes; a stampede across 200
// services restarting at once should not melt it.
type lookupEntry struct {
	svc Service
	ts  time.Time
}

var (
	lookupMu    sync.RWMutex
	lookupTable = map[string]lookupEntry{}
)

const lookupTTL = 5 * time.Minute

// Lookup returns Service metadata for slug. The collector resolves
// fleet members from the latest services.json mirror; this gives
// callers a runtime-fresh URL instead of a hardcoded constant.
//
// Designed for replacing hardcoded service URLs:
//
//	svc, err := graph.Lookup("go-js-proxy")
//	if err != nil { /* fall back to env or constant */ }
//	resp, err := client.Get(svc.URL + "/render")
//
// Lookup is cached for ~5 minutes; pass force=true semantics by
// calling Forget(slug) first if you need a fresh fetch.
func Lookup(slug string) (Service, error) {
	return LookupCtx(context.Background(), slug)
}

// LookupCtx is the context-aware Lookup. Honours deadlines.
func LookupCtx(ctx context.Context, slug string) (Service, error) {
	if slug == "" {
		return Service{}, ErrNotFound
	}
	lookupMu.RLock()
	if e, ok := lookupTable[slug]; ok && time.Since(e.ts) < lookupTTL {
		lookupMu.RUnlock()
		return e.svc, nil
	}
	lookupMu.RUnlock()

	s := ensureInit()
	if s.cfg.collectorURL == "" {
		return Service{}, ErrNotConfigured
	}

	url := s.cfg.collectorURL + "/lookup/" + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Service{}, err
	}
	if s.cfg.apiKey != "" {
		req.Header.Set("X-API-Key", s.cfg.apiKey)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Service{}, fmt.Errorf("graph lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Service{}, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Service{}, fmt.Errorf("graph lookup: status %d", resp.StatusCode)
	}
	var svc Service
	if err := json.NewDecoder(resp.Body).Decode(&svc); err != nil {
		return Service{}, fmt.Errorf("graph lookup decode: %w", err)
	}
	lookupMu.Lock()
	lookupTable[slug] = lookupEntry{svc: svc, ts: time.Now()}
	lookupMu.Unlock()
	return svc, nil
}

// Forget removes slug from the cache so the next Lookup re-fetches.
// Useful after a known deploy or in test setup.
func Forget(slug string) {
	lookupMu.Lock()
	delete(lookupTable, slug)
	lookupMu.Unlock()
}

package graph

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchIDStableOnRetryAndDistinctForNextBatch(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get(BatchIDHeader))
		if len(ids) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, r, _ := newTestSender(config{enabled: true, collectorURL: srv.URL, writerAPIKey: "key"}, 1)
	_, _ = r.push(Event{Path: "/same"})
	_, _ = r.push(Event{Path: "/same"})
	s.flush()
	s.flush()
	if len(ids) != 3 {
		t.Fatalf("got %d requests, want failed send, retry and next batch", len(ids))
	}
	if _, err := hex.DecodeString(ids[0]); err != nil || len(ids[0]) != 32 {
		t.Fatalf("invalid id %q", ids[0])
	}
	if ids[0] != ids[1] || ids[1] == ids[2] {
		t.Fatalf("batch IDs not stable and distinct: %v", ids)
	}
}

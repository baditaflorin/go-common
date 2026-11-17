package proxysupplier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestWebshareDirect_CatalogPageSize1000AcceptsFullPage(t *testing.T) {
	entries := make([]webshareEntry, webshareDirectDefaultPageSize)
	for i := range entries {
		entries[i] = webshareEntry{
			Username:     "user",
			Password:     "pass",
			ProxyAddress: "10.0.0.1",
			Port:         5_000 + i,
			Valid:        true,
		}
	}
	var requestedPageSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPageSize = r.URL.Query().Get("page_size")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(entries), Results: entries})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := &webshareDirectSupplier{apiKey: "test-key", baseURL: srv.URL, httpClient: srv.Client()}
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh with a full page: %v", err)
	}
	if want := strconv.Itoa(webshareDirectDefaultPageSize); requestedPageSize != want {
		t.Fatalf("requested page_size = %q, want %q", requestedPageSize, want)
	}
	s.mu.RLock()
	got := len(s.list)
	s.mu.RUnlock()
	if got != len(entries) {
		t.Fatalf("loaded %d entries, want all %d entries", got, len(entries))
	}
}

func TestWebshareDirect_CatalogRejectsResultsAboveRequestedPageSize(t *testing.T) {
	entries := make([]webshareEntry, webshareDirectDefaultPageSize+1)
	for i := range entries {
		entries[i] = webshareEntry{
			Username:     "user",
			Password:     "pass",
			ProxyAddress: "10.0.0.1",
			Port:         5_000 + i,
			Valid:        true,
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(entries), Results: entries})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := &webshareDirectSupplier{apiKey: "test-key", baseURL: srv.URL, httpClient: srv.Client()}
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("refresh accepted more results than the requested page size")
	}
}

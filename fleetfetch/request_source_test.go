package fleetfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestSourceOverridesEnvironmentDefault(t *testing.T) {
	t.Setenv(EnvSource, "commoncrawl")
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("source")
		w.Header().Set("X-FetchCache-Fetched-At", "2026-10-09T00:00:00Z")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := NewClient(WithCacheURL(srv.URL))
	ctx := WithRequestSource(context.Background(), SourceLive)
	if _, err := client.Get(ctx, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Fatalf("source query = %q, want explicit live override", seen)
	}
}

func TestRequestSourceCommonCrawlOverridesLiveEnvironment(t *testing.T) {
	t.Setenv(EnvSource, "live")
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("source")
		w.Header().Set("X-FetchCache-Fetched-At", "2026-10-09T00:00:00Z")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := NewClient(WithCacheURL(srv.URL))
	ctx := WithRequestSource(context.Background(), SourceCommonCrawl)
	if _, err := client.Get(ctx, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if seen != "commoncrawl" {
		t.Fatalf("source query = %q, want commoncrawl", seen)
	}
}

func TestExplicitClientSourceOverridesRequestContext(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("source")
		w.Header().Set("X-FetchCache-Fetched-At", "2026-10-09T00:00:00Z")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := NewClient(WithCacheURL(srv.URL), WithSource(SourceCommonCrawl))
	ctx := WithRequestSource(context.Background(), SourceLive)
	if _, err := client.Get(ctx, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if seen != "commoncrawl" {
		t.Fatalf("source query = %q, want explicit client source commoncrawl", seen)
	}
}

func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Source
		ok    bool
	}{
		{input: "live", want: SourceLive, ok: true},
		{input: " LIVE ", want: SourceLive, ok: true},
		{input: "commoncrawl", want: SourceCommonCrawl, ok: true},
		{input: " COMMONCRAWL ", want: SourceCommonCrawl, ok: true},
		{input: "js", want: SourceLive, ok: false},
	} {
		got, err := ParseSource(tc.input)
		if (err == nil) != tc.ok {
			t.Fatalf("ParseSource(%q) error = %v, want ok=%t", tc.input, err, tc.ok)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseSource(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

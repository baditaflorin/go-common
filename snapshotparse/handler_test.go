package snapshotparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerReceiptsAndBounds(t *testing.T) {
	html := []byte("<p>caf\xe9</p>")
	digest := fmt.Sprintf("%x", sha256.Sum256(html))
	var received Input
	h := Handler("example-parser", "1.2.3", func(_ context.Context, in Input) (Result, error) {
		received = in
		text, err := in.UTF8()
		return Result{Status: Parsed, Data: string(text)}, err
	})
	request := func(body []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/parse", bytes.NewReader(body))
		r.Header.Set("Content-Type", "text/html; charset=windows-1252")
		r.Header.Set("X-Source-URL", "https://example.test/about?lang=fr")
		r.Header.Set("X-Source-SHA256", digest)
		r.Header.Set("X-Capture-ID", "cap_original")
		return r
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(html))
	var got struct {
		Schema string            `json:"schema_version"`
		Parser map[string]string `json:"parser"`
		Input  map[string]string `json:"input"`
		Status string            `json:"status"`
		Data   string            `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d %s (%v)", w.Code, w.Body, err)
	}
	if !bytes.Equal(received.HTML, html) || got.Schema != SchemaVersion || got.Parser["id"] != "example-parser" || got.Parser["version"] != "1.2.3" || got.Input["url"] != received.URL || got.Input["sha256"] != digest || got.Input["capture_id"] != "cap_original" || got.Status != Parsed || got.Data != "<p>café</p>" {
		t.Fatalf("wrong input or receipt: %+v", got)
	}
	reset := Input{}
	for _, tc := range []struct {
		name string
		req  func() *http.Request
		code int
	}{
		{"mismatched hash", func() *http.Request { return request([]byte("different")) }, 422},
		{"empty body", func() *http.Request { return request(nil) }, 400},
		{"oversized", func() *http.Request { return request(make([]byte, MaxBytes+1)) }, 413},
		{"wrong method", func() *http.Request {
			r := request(html)
			r.Method = http.MethodGet
			return r
		}, 405},
		{"encoded body", func() *http.Request {
			r := request(html)
			r.Header.Set("Content-Encoding", "gzip")
			return r
		}, 415},
		{"missing capture", func() *http.Request {
			r := request(html)
			r.Header.Del("X-Capture-ID")
			return r
		}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received = reset
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req())
			if w.Code != tc.code || received.HTML != nil {
				t.Fatalf("status=%d, parser called=%t", w.Code, received.HTML != nil)
			}
		})
	}
	w = httptest.NewRecorder()
	Handler("example-parser", "1.2.3", func(context.Context, Input) (Result, error) {
		return Result{}, errors.New("confidential parsing context")
	}).ServeHTTP(w, request(html))
	if w.Code != 422 || bytes.Contains(w.Body.Bytes(), []byte("confidential")) {
		t.Fatal("parser error was improperly surfaced")
	}
}

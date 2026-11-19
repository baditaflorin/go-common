package capturestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestCreateDerivationBatchPreservesBytesAndChecksOrderedReceipts(t *testing.T) {
	ctx := context.Background()
	items := []DerivationWrite{
		{Metadata: DerivationMetadata{IdempotencyKey: "first"}, Body: []byte("{ \"a\": 1 }\n")},
		{Metadata: DerivationMetadata{IdempotencyKey: "second"}, Body: []byte(`{"b":true}`)},
	}
	results := make([]DerivationResult, len(items))
	for i, item := range items {
		results[i].Derivation = Derivation{ID: fmt.Sprintf("der_%d", i), CaptureID: "cap_1",
			DerivationMetadata: item.Metadata, SHA256: fmt.Sprintf("%x", sha256.Sum256(item.Body)), BodyBytes: int64(len(item.Body)), GzipBytes: 23}
	}
	mode := "success"
	c, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DerivationBatch
		if r.Method != http.MethodPost || r.URL.Path != "/v1/captures/cap_1/derivations/batch" ||
			json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Items) != len(items) ||
			!bytes.Equal(req.Items[0].Body, items[0].Body) || r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Error("batch request lost data or authentication")
		}
		out := results
		switch mode {
		case "short":
			out = out[:1]
		case "reordered":
			out = []DerivationResult{out[1], out[0]}
		case "http error":
			w.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}))
	defer server.Close()
	if got, err := c.CreateDerivationBatch(ctx, "cap_1", items); err != nil || got[1] != results[1] {
		t.Fatalf("create: %v, %v", got, err)
	}
	for _, invalid := range []string{"short", "reordered", "http error"} {
		mode = invalid
		if _, err := c.CreateDerivationBatch(ctx, "cap_1", items); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	for _, bad := range [][]DerivationWrite{nil, make([]DerivationWrite, MaxDerivationBatchItems+1), {{Body: []byte("invalid")}},
		{{Body: append(append([]byte(`{"v":"`), bytes.Repeat([]byte("x"), MaxDerivationBatchBytes*3/4)...), []byte(`"}`)...)}}} {
		if _, err := c.CreateDerivationBatch(ctx, "cap_1", bad); err == nil {
			t.Fatal("accepted invalid or oversized batch")
		}
	}
}

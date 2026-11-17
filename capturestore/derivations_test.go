package capturestore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestDerivationRoundTripAndChecksum(t *testing.T) {
	body := json.RawMessage(`{"data":{"text":"Company evidence"}}`)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	m := DerivationMetadata{IdempotencyKey: "parse:1", InputSHA256: digest, ParserID: "parser", ParserVersion: "1.0.0", OptionsSHA256: digest, Status: "parsed"}
	d := Derivation{ID: "derived_1", DerivationMetadata: m, CaptureID: "cap_1", SHA256: digest, BodyBytes: int64(len(body)), GzipBytes: 99}
	tampered := false
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Fatal("auth missing")
		}
		if r.Method == "POST" {
			if r.URL.Path != "/v1/captures/cap_1/derivations" {
				t.Errorf("path %s", r.URL.Path)
			}
			raw, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Derivation-Metadata"))
			var got DerivationMetadata
			_ = json.Unmarshal(raw, &got)
			b, _ := io.ReadAll(r.Body)
			if got != m || string(b) != string(body) {
				t.Errorf("input changed")
			}
			_ = json.NewEncoder(w).Encode(DerivationResult{Derivation: d})
			return
		}
		if r.URL.Path == "/v1/derivations/derived_1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"derivation": d})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Derivation-SHA256", digest)
		if tampered {
			_, _ = w.Write([]byte(`{"invalid":"checksummed body changed"}`))
			return
		}
		_, _ = w.Write(body)
	}))
	defer s.Close()
	got, err := c.CreateDerivation(context.Background(), "cap_1", m, body)
	if err != nil || got.Derivation.ID != d.ID {
		t.Fatalf("create %+v %v", got, err)
	}
	meta, err := c.GetDerivation(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.DerivationBody(context.Background(), meta)
	if err != nil || string(b) != string(body) {
		t.Fatalf("body %s %v", b, err)
	}
	tampered = true
	if _, err = c.DerivationBody(context.Background(), meta); err == nil {
		t.Fatal("tampered body accepted")
	}
}

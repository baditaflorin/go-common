package capturestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func validDerivationFixture() (DerivationMetadata, []byte, Derivation) {
	body := []byte("{\"data\":\"evidence\"}")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	m := DerivationMetadata{IdempotencyKey: "parse:1", InputSHA256: digest, ParserID: "parser", ParserVersion: "1.0.0", OptionsSHA256: digest, Status: "parsed"}
	d := Derivation{ID: "derived_1", DerivationMetadata: m, CaptureID: "cap_1", Namespace: "pilot", URL: "https://example.test/", SHA256: digest, BodyBytes: int64(len(body)), GzipBytes: 19, CreatedAt: "2026-11-17T00:00:00Z"}
	return m, body, d
}

func TestListDerivationsFiltersPaginationAndAuthorization(t *testing.T) {
	want := Derivation{ID: "derived_1", CaptureID: "cap_1", Namespace: "pilot", URL: "https://example.test/", DerivationMetadata: DerivationMetadata{ParserID: "site-capsule", ParserVersion: "0.2.1"}}
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/derivations" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Error("scoped owner token was not propagated")
		}
		q := r.URL.Query()
		for key, value := range map[string]string{"namespace": "pilot", "domain": "example.test", "capture_id": "cap_1", "parser_id": "site-capsule", "limit": "7", "offset": "3"} {
			if q.Get(key) != value {
				t.Errorf("%s=%q; want %q", key, q.Get(key), value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(DerivationList{Derivations: []Derivation{want}, Total: 19, Limit: 7, Offset: 3})
	}))
	defer s.Close()
	got, err := c.ListDerivations(context.Background(), DerivationListOptions{Namespace: "pilot", Domain: "example.test", CaptureID: "cap_1", ParserID: "site-capsule", Limit: 7, Offset: 3})
	if err != nil || got.Total != 19 || got.Limit != 7 || got.Offset != 3 || len(got.Derivations) != 1 || got.Derivations[0].ID != want.ID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestDerivationIDsAndPaginationRejectBeforeNetwork(t *testing.T) {
	requests := 0
	c, s := testClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer s.Close()
	m, body, d := validDerivationFixture()
	if _, err := c.CreateDerivation(context.Background(), "bad id", m, body); err == nil {
		t.Fatal("invalid capture ID accepted")
	}
	if _, err := c.GetDerivation(context.Background(), "bad id"); err == nil {
		t.Fatal("invalid derivation ID accepted")
	}
	d.ID = "../bad"
	if _, err := c.DerivationBody(context.Background(), d); err == nil {
		t.Fatal("invalid body ID accepted")
	}
	for _, o := range []DerivationListOptions{{Limit: -1}, {Limit: 101}, {Offset: -1}} {
		if _, err := c.ListDerivations(context.Background(), o); err == nil {
			t.Fatalf("invalid pagination accepted: %+v", o)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid inputs made %d network requests", requests)
	}
}

func TestCreateDerivationRejectsInvalidInputBeforeNetwork(t *testing.T) {
	requests := 0
	c, s := testClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer s.Close()
	m, body, _ := validDerivationFixture()
	cases := []struct {
		name    string
		capture string
		meta    DerivationMetadata
		body    []byte
	}{
		{"invalid-json", "cap_1", m, []byte("{bad")},
		{"bad-input-digest", "cap_1", DerivationMetadata{IdempotencyKey: m.IdempotencyKey, InputSHA256: "bad", ParserID: m.ParserID, ParserVersion: m.ParserVersion, OptionsSHA256: m.OptionsSHA256}, body},
		{"bad-options-digest", "cap_1", DerivationMetadata{IdempotencyKey: m.IdempotencyKey, InputSHA256: m.InputSHA256, ParserID: m.ParserID, ParserVersion: m.ParserVersion, OptionsSHA256: "bad"}, body},
		{"missing-idempotency", "cap_1", DerivationMetadata{InputSHA256: m.InputSHA256, ParserID: m.ParserID, ParserVersion: m.ParserVersion, OptionsSHA256: m.OptionsSHA256}, body},
		{"metadata-too-large", "cap_1", DerivationMetadata{IdempotencyKey: m.IdempotencyKey, InputSHA256: m.InputSHA256, ParserID: m.ParserID, ParserVersion: strings.Repeat("v", 5000), OptionsSHA256: m.OptionsSHA256}, body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateDerivation(context.Background(), tc.capture, tc.meta, tc.body); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if _, err := c.CreateDerivation(context.Background(), "cap_1", m, make([]byte, MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized body accepted")
	}
	if requests != 0 {
		t.Fatalf("invalid input made %d network requests", requests)
	}
}

func TestCreateDerivationRejectsReceiptBindingMismatches(t *testing.T) {
	m, body, base := validDerivationFixture()
	cases := []struct {
		name   string
		mutate func(*Derivation)
	}{
		{"id", func(d *Derivation) { d.ID = "bad id" }},
		{"capture", func(d *Derivation) { d.CaptureID = "cap_2" }},
		{"metadata", func(d *Derivation) { d.ParserVersion = "2.0.0" }},
		{"body-size", func(d *Derivation) { d.BodyBytes++ }},
		{"checksum", func(d *Derivation) { d.SHA256 = strings.Repeat("0", 64) }},
		{"gzip-size", func(d *Derivation) { d.GzipBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := base
			tc.mutate(&want)
			c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(DerivationResult{Derivation: want})
			}))
			defer s.Close()
			if _, err := c.CreateDerivation(context.Background(), "cap_1", m, body); err == nil || err.Error() != "capturestore: invalid derivation receipt" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGetDerivationRejectsMismatchedReceiptID(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"derivation":{"id":"derived_other"}}`)
	}))
	defer s.Close()
	if _, err := c.GetDerivation(context.Background(), "derived_1"); err == nil || err.Error() != "capturestore: invalid derivation receipt" {
		t.Fatalf("err=%v", err)
	}
}

func TestDerivationBodyRejectsMalformedAndMismatchedReceipts(t *testing.T) {
	_, body, d := validDerivationFixture()
	badJSON := []byte("{\"data\":xevidence\"}") // same length as the valid body
	if len(badJSON) != len(body) {
		t.Fatalf("fixture lengths %d != %d", len(badJSON), len(body))
	}
	cases := []struct {
		name, contentType, checksum string
		payload                     []byte
		error                       string
	}{
		{"content-type", "text/html", d.SHA256, body, "invalid derivation body receipt"},
		{"header-checksum", "application/json", strings.Repeat("0", 64), body, "invalid derivation body receipt"},
		{"malformed-json", "application/json", d.SHA256, badJSON, "invalid derivation body"},
		{"body-checksum", "application/json", d.SHA256, []byte("{\"data\":\"evidencX\"}"), "derivation checksum mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("X-Derivation-SHA256", tc.checksum)
				_, _ = w.Write(tc.payload)
			}))
			defer s.Close()
			_, err := c.DerivationBody(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("err=%v want containing %q", err, tc.error)
			}
		})
	}
}

func TestDerivationServerErrorsDoNotLeakResponseBody(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "private derivation payload test-scoped-key")
	}))
	defer s.Close()
	_, err := c.ListDerivations(context.Background(), DerivationListOptions{})
	if err == nil || strings.Contains(err.Error(), "private derivation payload") || strings.Contains(err.Error(), "test-scoped-key") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("unsafe or unexpected error: %v", err)
	}
}

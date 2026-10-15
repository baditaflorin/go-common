package apikey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKeyIDMatchesTheKeystore(t *testing.T) {
	// go_apikey_service keyIDOf: "kid_" + first 24 hex of SHA-256(key). The
	// value is pinned (computed independently) so a change on either side is
	// caught by a test, not by a failed revoke in production.
	got := KeyID("ak_example_value")
	if got != "kid_c1619dca5696992c534d0dae" {
		t.Fatalf("KeyID = %q", got)
	}
	if got != KeyID("ak_example_value") || got == KeyID("ak_example_valuf") {
		t.Fatal("must be deterministic and distinct per key")
	}
}

func TestRevokeIDSendsKeyID(t *testing.T) {
	resetBreakers()
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"status":"success","data":{"revoked":true}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, AdminToken: "adm", HTTPClient: srv.Client(), UserAgent: "t"}
	id := KeyID("ak_x")
	if ok, err := c.RevokeID(context.Background(), id); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if gotBody["key_id"] != id || gotBody["key"] != "" {
		t.Fatalf("body %v", gotBody)
	}
	if _, err := c.RevokeID(context.Background(), "ak_x"); err == nil {
		t.Fatal("a raw key passed to RevokeID must be refused, never sent as an id")
	}
}

func TestListDecodesKeyIDAndPrefix(t *testing.T) {
	resetBreakers()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"keys":[{"key":"","key_id":"kid_abc","key_prefix":"ak_12345…","user":"u","scope":"s"}]}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, AdminToken: "adm", HTTPClient: srv.Client(), UserAgent: "t"}
	rows, err := c.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].KeyID != "kid_abc" || rows[0].KeyPrefix != "ak_12345…" {
		t.Fatalf("%+v %v", rows, err)
	}
}

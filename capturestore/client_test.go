package capturestore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	c, e := New(Config{URL: s.URL, Token: "test-scoped-key", Transport: s.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	return c, s
}
func TestCreateEncodesMetadataAndSendsRawBody(t *testing.T) {
	body := []byte("page bytes")
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != capturesPath {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Errorf("missing bearer auth")
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		raw, e := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Capture-Metadata"))
		if e != nil {
			t.Errorf("metadata encoding: %v", e)
		}
		var got Metadata
		if e = json.Unmarshal(raw, &got); e != nil {
			t.Errorf("metadata json: %v", e)
		}
		if got.Namespace != "ns" || got.BodyKind != "website_response" {
			t.Errorf("metadata: %+v", got)
		}
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, body) {
			t.Errorf("body %q", b)
		}
		w.Header().Set("Content-Type", "application/json")
		sum := sha256.Sum256(body)
		receipt := CreateResult{Capture: Capture{ID: "cap_1", Metadata: Metadata{Namespace: "ns", IdempotencyKey: "idem", SourceService: "cache"}, BodyBytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), GzipBytes: 1}}
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	defer s.Close()
	got, e := c.Create(context.Background(), Metadata{IdempotencyKey: "idem", Namespace: "ns", SourceService: "cache", URL: "https://example.test", BodyKind: "website_response"}, body)
	if e != nil {
		t.Fatal(e)
	}
	if got.Capture.ID != "cap_1" || got.Reused {
		t.Fatalf("result %+v", got)
	}
}
func TestCreateEnforcesLimitsBeforeNetwork(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected request") }))
	defer s.Close()
	m := Metadata{IdempotencyKey: "i", Namespace: "n", SourceService: "s", URL: "https://e.test", BodyKind: "website_response"}
	if _, e := c.Create(context.Background(), m, make([]byte, MaxBodyBytes+1)); e == nil || !strings.Contains(e.Error(), "8 MiB") {
		t.Fatalf("body error %v", e)
	}
	m.Settings = map[string]string{"huge": strings.Repeat("x", MaxMetadataBytes)}
	if _, e := c.Create(context.Background(), m, nil); e == nil || !strings.Contains(e.Error(), "16 KiB") {
		t.Fatalf("metadata error %v", e)
	}
}
func TestNewRequiresHTTPSOriginAndSafeCredentialSource(t *testing.T) {
	for _, u := range []string{"http://store.test", "https://user:pass@store.test", "https://store.test/path", "https://store.test/?q=1"} {
		if _, e := New(Config{URL: u, Token: "x"}); e == nil {
			t.Errorf("accepted %q", u)
		}
	}
	if _, e := New(Config{URL: "https://store.test", Token: "a\nb"}); e == nil {
		t.Error("accepted newline token")
	}
}
func TestHTTPErrorDoesNotLeakResponseBody(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, "private body test-scoped-key")
	}))
	defer s.Close()
	_, e := c.Get(context.Background(), "cap")
	if e == nil || strings.Contains(e.Error(), "private body") || strings.Contains(e.Error(), "test-scoped-key") {
		t.Fatalf("unsafe error %v", e)
	}
}
func TestDownloadDecompressesAndChecksRawChecksum(t *testing.T) {
	raw := []byte("original response")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	makeServer := func(checksum string) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "identity" {
				t.Errorf("Accept-Encoding=%q", r.Header.Get("Accept-Encoding"))
			}
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			_, _ = gz.Write(raw)
			_ = gz.Close()
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Disposition", `attachment; filename="cap.response.gz"`)
			w.Header().Set("X-Capture-SHA256", checksum)
			w.Header().Set("Content-Length", stringInt(len(b.Bytes())))
			_, _ = w.Write(b.Bytes())
		}))
	}
	s := makeServer(digest)
	c, e := New(Config{URL: s.URL, Token: "t", Transport: s.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	got, e := c.Download(context.Background(), "cap")
	s.Close()
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("body %q", got)
	}
	s = makeServer(strings.Repeat("0", 64))
	c, e = New(Config{URL: s.URL, Token: "t", Transport: s.Client().Transport})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.Download(context.Background(), "cap")
	s.Close()
	if e == nil || !strings.Contains(e.Error(), "checksum mismatch") {
		t.Fatalf("checksum result %v", e)
	}
}
func stringInt(n int) string { return fmt.Sprint(n) }
func TestCreateRejectsMismatchedReceipt(t *testing.T) {
	body := []byte("page")
	sum := sha256.Sum256(body)
	cases := []struct {
		name              string
		namespace, digest string
	}{
		{"wrong owner label", "other", hex.EncodeToString(sum[:])},
		{"wrong body digest", "ns", strings.Repeat("0", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got := CreateResult{Capture: Capture{ID: "cap", Metadata: Metadata{Namespace: tc.namespace, IdempotencyKey: "idem", SourceService: "cache"}, BodyBytes: int64(len(body)), SHA256: tc.digest, GzipBytes: 8}}
				_ = json.NewEncoder(w).Encode(got)
			}))
			defer s.Close()
			m := Metadata{IdempotencyKey: "idem", Namespace: "ns", SourceService: "cache", URL: "https://example.test", BodyKind: "website_response"}
			if _, err := c.Create(context.Background(), m, body); err == nil || err.Error() != "capturestore: invalid create receipt" {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
func TestReadEndpointsAndBoundedListFilters(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Error("missing bearer auth")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/captures/cap_1":
			io.WriteString(w, `{"id":"cap_1"}`)
		case "/v1/captures":
			if r.URL.Query().Get("namespace") != "ns" || r.URL.Query().Get("provider") != "webshare" || r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("offset") != "4" {
				t.Errorf("list query %v", r.URL.Query())
			}
			io.WriteString(w, `{"captures":[],"total":12,"limit":100,"offset":4}`)
		case "/v1/storage":
			io.WriteString(w, `{"observations":3,"unique_objects":2}`)
		case "/v1/compare":
			if r.URL.Query().Get("left") != "cap_1" || r.URL.Query().Get("right") != "cap_2" {
				t.Errorf("compare query %v", r.URL.Query())
			}
			io.WriteString(w, `{"same_body":true}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	if _, err := c.Get(context.Background(), "cap_1"); err != nil {
		t.Fatal(err)
	}
	listed, err := c.List(context.Background(), ListOptions{Namespace: "ns", Provider: "webshare", Limit: 100, Offset: 4})
	if err != nil || listed.Total != 12 {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	stats, err := c.Storage(context.Background())
	if err != nil || stats.UniqueObjects != 2 {
		t.Fatalf("storage=%+v err=%v", stats, err)
	}
	diff, err := c.Compare(context.Background(), "cap_1", "cap_2")
	if err != nil || !diff.SameBody {
		t.Fatalf("compare=%+v err=%v", diff, err)
	}
	if _, err := c.List(context.Background(), ListOptions{Limit: 101}); err == nil {
		t.Fatal("accepted list limit above 100")
	}
}
func TestLargeListFitsBoundedResponseLimit(t *testing.T) {
	row := "{\"id\":\"cap\",\"padding\":\"" + strings.Repeat("x", 12000) + "\"}"
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = row
	}
	raw := "{\"captures\":[" + strings.Join(rows, ",") + "],\"total\":100,\"limit\":100,\"offset\":0}"
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
	}))
	defer s.Close()
	got, err := c.List(context.Background(), ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Captures) != 100 {
		t.Fatalf("got %d rows", len(got.Captures))
	}
}

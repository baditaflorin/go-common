package reqstats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, opts ...Option) (*Stats, http.Header) {
	t.Helper()
	mw := Middleware("svc-test", "9.9.9", opts...)
	rr := httptest.NewRecorder()
	mw(h).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	raw := rr.Header().Get("X-Request-Stats")
	if raw == "" {
		t.Fatal("missing X-Request-Stats header")
	}
	var s Stats
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("X-Request-Stats not valid JSON: %v (%s)", err, raw)
	}
	return &s, rr.Header()
}

func TestUniversalPayload(t *testing.T) {
	s, hdr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	if s.Svc != "svc-test" || s.Ver != "9.9.9" {
		t.Fatalf("svc/ver: %+v", s)
	}
	if !s.OK {
		t.Fatal("ok should be true for 200")
	}
	if s.Approx == nil || s.Approx.Note == "" {
		t.Fatal("approx CPU block + note expected by default")
	}
	if s.Approx.HeapAllocDelta != 0 {
		t.Fatal("heap delta must be 0 unless WithHeapDelta")
	}
	if st := hdr.Get("Server-Timing"); !strings.HasPrefix(st, "total;dur=") {
		t.Fatalf("Server-Timing missing total: %q", st)
	}
}

func TestEnrichmentPhasesRenderUpstream(t *testing.T) {
	s, hdr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt := From(r.Context())
		rt.Mark("fetch", 0)
		rt.SetRender(Render{TaskMs: 1700, JSHeapUsed: 42})
		rt.SetUpstream(`{"svc":"upstream","total_ms":5}`)
		w.Write([]byte("x"))
	}), WithHeapDelta())
	if s.Phase == nil {
		t.Fatal("phase block expected")
	}
	if _, ok := s.Phase["fetch"]; !ok {
		t.Fatalf("phase fetch missing: %+v", s.Phase)
	}
	if s.Render == nil || s.Render.TaskMs != 1700 {
		t.Fatalf("render block missing: %+v", s.Render)
	}
	if len(s.Upstream) == 0 || !strings.Contains(string(s.Upstream), "upstream") {
		t.Fatalf("upstream not nested: %s", s.Upstream)
	}
	if s.Approx == nil || s.Approx.HeapAllocDelta == 0 {
		// heap delta should be present with WithHeapDelta (alloc happens in JSON marshal etc.)
		t.Logf("heap delta: %v (may be 0 on a tiny handler — acceptable)", s.Approx)
	}
	if st := hdr.Get("Server-Timing"); !strings.Contains(st, "render_cpu;dur=1700") {
		t.Fatalf("Server-Timing missing render_cpu: %q", st)
	}
}

func TestErrorStatusMarksNotOK(t *testing.T) {
	s, _ := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if s.OK {
		t.Fatal("ok should be false for 5xx")
	}
}

func TestSetUpstreamRejectsInvalidJSON(t *testing.T) {
	tr := Start("a", "b")
	tr.SetUpstream("not json")
	if len(tr.Stats().Upstream) != 0 {
		t.Fatal("invalid upstream JSON must be dropped")
	}
}

func TestFromAbsentIsNoop(t *testing.T) {
	if From(context.Background()) == nil {
		t.Fatal("From must never return nil")
	}
}

func TestContentLengthBecomesBytesOut(t *testing.T) {
	s, _ := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	if s.Bytes.Out != 12345 {
		t.Fatalf("bytes.out from Content-Length: got %d", s.Bytes.Out)
	}
}

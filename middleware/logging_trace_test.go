package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/baditaflorin/go-common/logx"
)

// With a logx logger installed, the access log of a traced request carries the
// trace id, so a log line can be joined to its trace.
func TestAccessLogCarriesTheTraceIDWithLogx(t *testing.T) {
	var buf bytes.Buffer
	saved := defaultLogger
	SetLogger(logx.New(logx.Options{Service: "go_example", Version: "1.0.0", Output: &buf}))
	t.Cleanup(func() { defaultLogger = saved })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	req := httptest.NewRequest(http.MethodGet, "/v1/items", nil)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0xbb},
		SpanID:     trace.SpanID{9, 8, 7, 6, 5, 4, 3, 2},
		TraceFlags: trace.FlagsSampled,
	})
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(trace.ContextWithSpanContext(req.Context(), sc)))

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not one JSON line: %v\n%s", err, buf.String())
	}
	if m["trace_id"] != "aa0102030405060708090a0b0c0d0ebb" || m["service"] != "go_example" || m["msg"] != "request_completed" ||
		m["status"] != float64(http.StatusTeapot) || m["path"] != "/v1/items" {
		t.Fatalf("access log = %v", m)
	}
}

func TestSetLoggerIgnoresNil(t *testing.T) {
	saved := defaultLogger
	t.Cleanup(func() { defaultLogger = saved })
	SetLogger(nil)
	if defaultLogger != saved {
		t.Fatal("SetLogger(nil) replaced the logger")
	}
}

func TestAccessLogOutputIsUnchangedByTheContextCalls(t *testing.T) {
	var buf bytes.Buffer
	saved := defaultLogger
	SetLogger(captureLogger(&buf, -4))
	t.Cleanup(func() { defaultLogger = saved })
	Logging(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !strings.Contains(buf.String(), `"msg":"request_completed"`) || strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("plain access log = %s", buf.String())
	}
}

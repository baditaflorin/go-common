package telemetry

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type transportTestFunc func(*http.Request) (*http.Response, error)

func (f transportTestFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportPreservesCallerHeaders(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header["X-Multi"] = []string{"first", "second"}
	req.Header.Set("Authorization", "Bearer synthetic-test")
	before := req.Header.Clone()
	rt := NewTransport(transportTestFunc(func(out *http.Request) (*http.Response, error) {
		if out == req {
			t.Error("transport received caller request")
		}
		if out.Header.Get("Traceparent") == "" {
			t.Error("missing propagated trace")
		}
		out.Header["X-Multi"][0] = "changed"
		out.Header.Set("Authorization", "changed")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !reflect.DeepEqual(req.Header, before) {
		t.Fatalf("caller headers changed: %#v", req.Header)
	}
}

// BenchmarkTransportHeaders includes populated caller headers, whose cloning
// cost is otherwise hidden by benchmarks of an empty request.
func BenchmarkTransportHeaders(b *testing.B) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer synthetic-test")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "benchmark")
	response := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
	rt := NewTransport(transportTestFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.RoundTrip(req); err != nil {
			b.Fatal(err)
		}
	}
}

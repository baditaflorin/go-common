package tracehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportPreservesTracePrivacyAndCallerRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
	}{
		{"success", 200, nil},
		{"server failure", 503, nil},
		{"transport error", 0, errors.New("private-error-value")},
		{"nil response", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
			otel.SetTracerProvider(provider)
			otel.SetTextMapPropagator(propagation.TraceContext{})
			t.Cleanup(func() {
				_ = provider.Shutdown(context.Background())
				otel.SetTracerProvider(previousProvider)
				otel.SetTextMapPropagator(previousPropagator)
			})
			req := httptest.NewRequest(http.MethodGet, "https://private-host.invalid/private-path?secret=private-query", nil)
			req.Header.Set("X-Private", "private-header")
			before := req.Header.Clone()
			var received *http.Request
			var expectedResponse *http.Response
			if tc.status > 0 {
				expectedResponse = &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("private-body"))}
			}
			rt := NewTransport(roundTripFunc(func(out *http.Request) (*http.Response, error) {
				received = out
				out.Header.Set("X-Transport", "private-change")
				return expectedResponse, tc.err
			}))
			resp, err := rt.RoundTrip(req)
			if resp != expectedResponse || err != tc.err || received == req || !reflect.DeepEqual(req.Header, before) {
				t.Fatal("wrapper changed response/error identity or caller request")
			}
			if received.Header.Get("traceparent") == "" || !trace.SpanContextFromContext(received.Context()).IsValid() {
				t.Fatal("client trace context did not reach the transport")
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 || spans[0].SpanKind != trace.SpanKindClient || spans[0].InstrumentationScope.Name != instrumentationScope {
				t.Fatalf("client span identity changed: %+v", spans)
			}
			wantCode := codes.Unset
			if tc.err != nil || tc.status >= 500 {
				wantCode = codes.Error
			}
			if spans[0].Status.Code != wantCode {
				t.Errorf("status=%v, want %v", spans[0].Status.Code, wantCode)
			}
			for _, attr := range spans[0].Attributes {
				if strings.Contains(attr.Value.String(), "private-") {
					t.Errorf("private request, response or error text leaked into %s", attr.Key)
				}
			}
		})
	}
}

type closingTransport struct{ closed bool }

func (c *closingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (c *closingTransport) CloseIdleConnections()                           { c.closed = true }

func TestTransportDefaultAndCleanup(t *testing.T) {
	if rt := NewTransport(nil).(*roundTripper); rt.base != http.DefaultTransport {
		t.Fatal("nil transport no longer falls back to http.DefaultTransport")
	}
	base := &closingTransport{}
	NewTransport(base).(interface{ CloseIdleConnections() }).CloseIdleConnections()
	if !base.closed {
		t.Fatal("idle connection cleanup was swallowed")
	}
	NewTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })).(interface{ CloseIdleConnections() }).CloseIdleConnections()
}

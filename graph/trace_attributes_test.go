package graph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestMiddlewareAddsVerifiedCallerToActiveSpan(t *testing.T) {
	resetState(t)
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", "http://127.0.0.1:43181")
	t.Setenv("GRAPH_API_KEY", "writer-test-key")
	t.Setenv("GRAPH_TRUSTED_CALLER_IPS", "10.10.10.10")
	t.Setenv("GRAPH_FLUSH_INTERVAL", "3600")
	Init("html-proxy", "0.0.0")

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
		resetState(t)
	})

	_, span := provider.Tracer("test").Start(context.Background(), "HTTP GET", trace.WithSpanKind(trace.SpanKindServer))
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/fetch", nil).WithContext(trace.ContextWithSpan(context.Background(), span))
	request.RemoteAddr = "10.10.10.10:45678"
	request.Header.Set("X-Auth-User", "go_search_duck")
	response := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	attrs := map[string]any{}
	for _, attr := range ended[0].Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	if got := attrs["fleet.caller.id"]; got != "go_search_duck" {
		t.Errorf("fleet.caller.id = %v, want go_search_duck", got)
	}
	if got := attrs["fleet.caller.verified"]; got != true {
		t.Errorf("fleet.caller.verified = %v, want true", got)
	}
}

func TestMiddlewareDoesNotTrustCallerHeaderFromUntrustedPeer(t *testing.T) {
	resetState(t)
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", "http://127.0.0.1:43182")
	t.Setenv("GRAPH_API_KEY", "writer-test-key")
	t.Setenv("GRAPH_TRUSTED_CALLER_IPS", "10.10.10.10")
	t.Setenv("GRAPH_FLUSH_INTERVAL", "3600")
	Init("html-proxy", "0.0.0")

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
		resetState(t)
	})

	_, span := provider.Tracer("test").Start(context.Background(), "HTTP GET", trace.WithSpanKind(trace.SpanKindServer))
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/fetch", nil).WithContext(trace.ContextWithSpan(context.Background(), span))
	request.RemoteAddr = "10.10.10.11:45678"
	request.Header.Set("X-Auth-User", "go_search_duck")
	response := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	attrs := map[string]any{}
	for _, attr := range ended[0].Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	if got := attrs["fleet.caller.id"]; got != "unknown" {
		t.Errorf("fleet.caller.id = %v, want unknown", got)
	}
	if got := attrs["fleet.caller.verified"]; got != false {
		t.Errorf("fleet.caller.verified = %v, want false", got)
	}
}

func TestMiddlewareAddsCallerToSpanWhenGraphEventsAreDisabled(t *testing.T) {
	resetState(t)
	t.Setenv("GRAPH_ENABLED", "false")
	t.Setenv("GRAPH_TRUSTED_CALLER_IPS", "10.10.10.10")
	Init("html-proxy", "0.0.0")

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
		resetState(t)
	})

	_, span := provider.Tracer("test").Start(context.Background(), "HTTP GET", trace.WithSpanKind(trace.SpanKindServer))
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/fetch", nil).WithContext(trace.ContextWithSpan(context.Background(), span))
	request.RemoteAddr = "10.10.10.10:45678"
	request.Header.Set("X-Auth-User", "go_search_duck")
	response := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	attrs := map[string]any{}
	for _, attr := range ended[0].Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	if got := attrs["fleet.caller.id"]; got != "go_search_duck" {
		t.Errorf("fleet.caller.id = %v, want go_search_duck", got)
	}
	if got := attrs["fleet.caller.verified"]; got != true {
		t.Errorf("fleet.caller.verified = %v, want true", got)
	}
}

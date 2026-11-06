// Package tracehttp instruments HTTP clients using only the OpenTelemetry API.
// SDK and exporter setup belongs to telemetry, at the application entry point.
package tracehttp

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Keep the existing scope so moving the implementation does not change trace identity.
const instrumentationScope = "github.com/baditaflorin/go-common/telemetry"

// NewTransport adds a client span and injects W3C trace context. It records no
// URLs, query strings, request/response headers, or bodies.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &roundTripper{base: base}
}

type roundTripper struct{ base http.RoundTripper }

// CloseIdleConnections preserves optional transport lifecycle cleanup through
// the instrumentation wrapper.
func (rt *roundTripper) CloseIdleConnections() {
	if closer, ok := rt.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer(instrumentationScope).Start(req.Context(), "HTTP "+req.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", req.Method)),
	)
	defer span.End()
	clone := req.Clone(ctx) // Clone already makes a private copy of Header.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(clone.Header))
	resp, err := rt.base.RoundTrip(clone)
	if err != nil {
		span.SetStatus(codes.Error, "transport error")
		span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		return resp, err
	}
	if resp != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		if resp.StatusCode >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "server error")
		}
	}
	return resp, nil
}

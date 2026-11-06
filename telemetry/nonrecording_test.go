package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNonrecordingHTTPPreservesTracePropagationAndWriter(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()), sdktrace.WithSpanProcessor(recorder))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		_ = tp.Shutdown(context.Background())
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/work", nil)
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-00")
	HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w != rr {
			t.Error("nonrecording request unnecessarily wrapped its writer")
		}
		sc := trace.SpanContextFromContext(r.Context())
		if !sc.IsValid() || sc.TraceID().String() != "0123456789abcdef0123456789abcdef" || sc.IsSampled() {
			t.Errorf("lost unsampled trace context: %v", sc)
		}
		headers := http.Header{}
		otel.GetTextMapPropagator().Inject(r.Context(), propagation.HeaderCarrier(headers))
		if headers.Get("traceparent") == "" {
			t.Error("trace context cannot be propagated onward")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || len(recorder.Ended()) != 0 {
		t.Fatal("nonrecording response changed or emitted a span")
	}
}

func BenchmarkHTTPMiddlewareSampling(b *testing.B) {
	for _, sampled := range []bool{false, true} {
		name := "Nonrecording"
		sampler := sdktrace.NeverSample()
		if sampled {
			name = "Recording"
			sampler = sdktrace.AlwaysSample()
		}
		b.Run(name, func(b *testing.B) {
			tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
			previous := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			b.Cleanup(func() { otel.SetTracerProvider(previous); _ = tp.Shutdown(context.Background()) })
			h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest(http.MethodGet, "/work", nil)
			b.ReportAllocs()
			for b.Loop() {
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}

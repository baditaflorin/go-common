package telemetry

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHTTPPropagationAndSensitiveDataRedaction(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		_ = tp.Shutdown(context.Background())
	})

	server := httptest.NewServer(HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !trace.SpanContextFromContext(r.Context()).IsValid() {
			t.Error("server did not receive a valid trace context")
		}
		_, _ = io.WriteString(w, "ok")
	})))
	defer server.Close()

	ctx, root := Tracer("test").Start(context.Background(), "root")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/private/customer?token=never-export", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Private", "header-secret")
	client := &http.Client{Transport: NewTransport(http.DefaultTransport)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	root.End()

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("got %d ended spans, want root + client + server", len(spans))
	}
	var rootSpan, clientSpan, serverSpan sdktrace.ReadOnlySpan
	for _, span := range spans {
		switch span.Name() {
		case "root":
			rootSpan = span
		case "HTTP GET":
			if span.SpanKind() == trace.SpanKindClient {
				clientSpan = span
			} else if span.SpanKind() == trace.SpanKindServer {
				serverSpan = span
			}
		}
		for _, attr := range span.Attributes() {
			if strings.Contains(string(attr.Key), "url") || strings.Contains(string(attr.Key), "header") || strings.Contains(attr.Value.AsString(), "private") || strings.Contains(attr.Value.AsString(), "never-export") || strings.Contains(attr.Value.AsString(), "header-secret") {
				t.Errorf("span %q contains sensitive attribute %s=%q", span.Name(), attr.Key, attr.Value.AsString())
			}
		}
	}
	if rootSpan == nil || clientSpan == nil || serverSpan == nil {
		t.Fatalf("missing spans: root=%v client=%v server=%v", rootSpan != nil, clientSpan != nil, serverSpan != nil)
	}
	if clientSpan.Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Errorf("client parent %s != root span %s", clientSpan.Parent().SpanID(), rootSpan.SpanContext().SpanID())
	}
	if serverSpan.Parent().SpanID() != clientSpan.SpanContext().SpanID() {
		t.Errorf("server parent %s != client span %s", serverSpan.Parent().SpanID(), clientSpan.SpanContext().SpanID())
	}
	if rootSpan.SpanContext().TraceID() != serverSpan.SpanContext().TraceID() {
		t.Errorf("trace IDs differ across HTTP boundary")
	}
}

func TestProbeRequestsAreNotTraced(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})
	rr := httptest.NewRecorder()
	HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("probe emitted %d spans, want zero", got)
	}
}

func TestOTLPEndpointRequiresTLSByDefault(t *testing.T) {
	old := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")
	t.Cleanup(func() { _ = os.Setenv("OTEL_EXPORTER_OTLP_INSECURE", old) })
	_ = os.Unsetenv("OTEL_EXPORTER_OTLP_INSECURE")
	if _, err := normalizeEndpoint("http://collector:4318"); err != nil {
		t.Fatalf("normalize endpoint: %v", err)
	}
	cfg := &Config{OTLPEndpoint: "http://collector:4318", SampleRate: 0.1}
	if _, err := newProvider(cfg); err == nil {
		t.Fatal("expected cleartext OTLP to be rejected by default")
	}
	if _, err := normalizeEndpoint("https://user:password@collector:4318"); err == nil {
		t.Fatal("expected credentials in endpoint URL to be rejected")
	}
	if _, err := normalizeEndpoint("https://bad host/path?token=must-not-leak"); err == nil || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("invalid endpoint error leaked input: %v", err)
	}
}

func TestAutoOpenObserveConfigurationUsesScopedFleetSecret(t *testing.T) {
	cfg := &Config{SampleRate: 0.1}
	values := map[string]string{
		"FLEET_API_KEY": "service-key",
	}
	called := false
	err := configureOpenObserveFromFleetSecrets(cfg, func(key string) string { return values[key] }, func(_ context.Context, baseURL, apiKey string) (string, error) {
		called = true
		if baseURL != defaultFleetSecretsURL || apiKey != "service-key" {
			t.Fatalf("unexpected secret lookup configuration: baseURL=%q apiKey=%q", baseURL, apiKey)
		}
		return "o2oi_canary-only", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected the service-scoped secret lookup")
	}
	if cfg.OTLPEndpoint != defaultOpenObserveEndpoint {
		t.Fatalf("endpoint=%q, want %q", cfg.OTLPEndpoint, defaultOpenObserveEndpoint)
	}
	gotAuth := cfg.otlpHeaders["Authorization"]
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("default:o2oi_canary-only"))
	if gotAuth != wantAuth {
		t.Fatalf("Authorization header not encoded as OpenObserve org-token Basic auth")
	}
	if cfg.otlpHeaders["stream-name"] != "default" {
		t.Fatalf("stream-name=%q, want default", cfg.otlpHeaders["stream-name"])
	}
}

func TestAutoOpenObserveConfigurationFailsClosed(t *testing.T) {
	t.Run("no fleet key", func(t *testing.T) {
		cfg := &Config{}
		called := false
		err := configureOpenObserveFromFleetSecrets(cfg, func(string) string { return "" }, func(context.Context, string, string) (string, error) {
			called = true
			return "unused", nil
		})
		if err != nil || called || cfg.OTLPEndpoint != "" {
			t.Fatalf("unexpected auto configuration: called=%v endpoint=%q err=%v", called, cfg.OTLPEndpoint, err)
		}
	})
	t.Run("manual endpoint wins", func(t *testing.T) {
		cfg := &Config{OTLPEndpoint: "https://collector.example/v1/traces"}
		called := false
		err := configureOpenObserveFromFleetSecrets(cfg, func(string) string { return "service-key" }, func(context.Context, string, string) (string, error) {
			called = true
			return "unused", nil
		})
		if err != nil || called || cfg.OTLPEndpoint != "https://collector.example/v1/traces" {
			t.Fatalf("manual endpoint was not preserved: called=%v endpoint=%q err=%v", called, cfg.OTLPEndpoint, err)
		}
	})
	t.Run("insecure vault URL rejected", func(t *testing.T) {
		cfg := &Config{}
		values := map[string]string{"FLEET_API_KEY": "service-key", "FLEET_SECRETS_URL": "http://fleet-secrets.0exec.com"}
		called := false
		err := configureOpenObserveFromFleetSecrets(cfg, func(key string) string { return values[key] }, func(context.Context, string, string) (string, error) {
			called = true
			return "unused", nil
		})
		if err == nil || called || cfg.OTLPEndpoint != "" {
			t.Fatalf("expected HTTPS validation to fail closed: called=%v endpoint=%q err=%v", called, cfg.OTLPEndpoint, err)
		}
	})
	t.Run("unapproved vault host rejected", func(t *testing.T) {
		cfg := &Config{}
		values := map[string]string{"FLEET_API_KEY": "service-key", "FLEET_SECRETS_URL": "https://attacker.example"}
		called := false
		err := configureOpenObserveFromFleetSecrets(cfg, func(key string) string { return values[key] }, func(context.Context, string, string) (string, error) {
			called = true
			return "unused", nil
		})
		if err == nil || called || cfg.OTLPEndpoint != "" {
			t.Fatalf("expected vault host validation to fail closed: called=%v endpoint=%q err=%v", called, cfg.OTLPEndpoint, err)
		}
	})
}

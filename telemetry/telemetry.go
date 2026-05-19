// Package telemetry provides a single-call initialisation for all
// fleet-wide observability: fleet graph (edge recording), Prometheus
// metrics (promx.AutoWire), and optionally OpenTelemetry distributed
// tracing.
//
// Before this package, every main.go called graph.Init + promx.AutoWire
// separately. This collapses those into one line and adds OTel as an
// opt-in without changing any existing callers.
//
// Usage (minimal, no OTel):
//
//	func main() {
//	    cfg := config.Load("my-service", "v1.0.0")
//	    telemetry.Init(cfg.AppName, cfg.Version)
//	    // ... rest of setup
//	}
//
// Usage (with OTLP export to a local collector):
//
//	telemetry.Init(cfg.AppName, cfg.Version,
//	    telemetry.WithOTLP("http://otel-collector:4318"),
//	)
//
// Usage (with OTel trace propagation in every outbound request):
//
//	telemetry.Init(cfg.AppName, cfg.Version,
//	    telemetry.WithOTLP(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
//	    telemetry.WithSampleRate(0.1),
//	)
//
// Environment variables (all optional):
//
//	OTEL_EXPORTER_OTLP_ENDPOINT  override the OTLP endpoint from code
//	OTEL_SAMPLE_RATE             override sample rate (float 0.0–1.0)
//	OTEL_DISABLED                set to "true" to disable OTel even if configured
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/promx"
	"github.com/baditaflorin/go-common/safehttp"
)

// Config holds the resolved telemetry configuration.
type Config struct {
	// ServiceName is the service identifier, e.g. "go_extractor".
	ServiceName string
	// ServiceVersion is the build version, e.g. "v1.4.2".
	ServiceVersion string
	// OTLPEndpoint is the OTLP HTTP endpoint (empty = disabled).
	// Example: "http://otel-collector:4318"
	OTLPEndpoint string
	// SampleRate controls the OTel trace sample rate (0.0–1.0).
	// Default 1.0 (sample everything). In production use 0.05–0.1.
	SampleRate float64
	// Disabled disables OTel tracing completely even if OTLPEndpoint is set.
	Disabled bool

	// internal
	tp *sdktrace.TracerProvider
}

// Option is a functional option for Init.
type Option func(*Config)

// WithOTLP enables OpenTelemetry tracing via the OTLP HTTP protocol and
// sets the collector endpoint. The endpoint is typically
// "http://otel-collector:4318" (no /v1/traces suffix; the exporter
// appends it). Setting this to "" is equivalent to not calling WithOTLP.
func WithOTLP(endpoint string) Option {
	return func(c *Config) { c.OTLPEndpoint = endpoint }
}

// WithSampleRate sets the OTel trace sample rate (0.0=no sampling, 1.0=all).
// Default is 1.0. Env var OTEL_SAMPLE_RATE overrides this value.
func WithSampleRate(r float64) Option {
	return func(c *Config) { c.SampleRate = r }
}

// WithDisabled disables OTel tracing regardless of other options.
func WithDisabled() Option {
	return func(c *Config) { c.Disabled = true }
}

// Init initialises all fleet telemetry subsystems:
//  1. graph.Init — fleet edge-recording identity
//  2. promx.AutoWire — Prometheus collectors + default observers
//  3. safehttp.SetDefaultObserver — egress Prometheus observer
//  4. OTel TracerProvider (if WithOTLP was provided or OTEL_EXPORTER_OTLP_ENDPOINT is set)
//
// Init is idempotent within a process (subsequent calls update identity).
// Returns a *Config that callers can inspect; shutdown is via Shutdown().
func Init(serviceName, serviceVersion string, opts ...Option) *Config {
	cfg := &Config{
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		SampleRate:     1.0,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	// Environment variable overrides.
	if ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); ep != "" && cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = ep
	}
	if sr := os.Getenv("OTEL_SAMPLE_RATE"); sr != "" {
		if f, err := strconv.ParseFloat(sr, 64); err == nil {
			cfg.SampleRate = f
		}
	}
	if os.Getenv("OTEL_DISABLED") == "true" {
		cfg.Disabled = true
	}

	// 1. Fleet graph identity (always).
	graph.Init(serviceName, serviceVersion)

	// 2. Prometheus collectors (always).
	egressColl, _, _ := promx.AutoWire(serviceName, serviceVersion)
	safehttp.SetDefaultObserver(egressColl)

	// 3. OTel (optional).
	if !cfg.Disabled && cfg.OTLPEndpoint != "" {
		if tp, err := initOTel(cfg); err != nil {
			slog.Warn("telemetry: OTel init failed, continuing without tracing",
				"error", err,
				"endpoint", cfg.OTLPEndpoint)
		} else {
			cfg.tp = tp
			slog.Info("telemetry: OTel tracing enabled",
				"endpoint", cfg.OTLPEndpoint,
				"sample_rate", cfg.SampleRate)
		}
	}

	return cfg
}

// Shutdown flushes and shuts down the OTel TracerProvider. Should be
// called in a deferred function in main() after Init.
//
//	cfg := telemetry.Init(name, version, telemetry.WithOTLP(endpoint))
//	defer cfg.Shutdown(context.Background())
func (c *Config) Shutdown(ctx context.Context) {
	if c.tp != nil {
		if err := c.tp.Shutdown(ctx); err != nil {
			slog.Warn("telemetry: OTel shutdown error", "error", err)
		}
	}
}

// TracerProvider returns the configured OTel TracerProvider, or the
// global no-op provider if OTel was not initialised.
func (c *Config) TracerProvider() *sdktrace.TracerProvider {
	return c.tp
}

// ─── OTel setup ──────────────────────────────────────────────────────────

func initOTel(cfg *Config) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()

	// OTLP HTTP exporter — uses the endpoint set via WithOTLP or env var.
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.OTLPEndpoint),
		otlptracehttp.WithInsecure(), // TLS is terminated at the collector; internal mesh is trusted
	)
	if err != nil {
		return nil, err
	}

	// Resource describes this service to the collector.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		// Resource errors are non-fatal; fall back to default.
		res = resource.Default()
	}

	// Sampler: respect the configured rate.
	var sampler sdktrace.Sampler
	switch {
	case cfg.SampleRate <= 0:
		sampler = sdktrace.NeverSample()
	case cfg.SampleRate >= 1:
		sampler = sdktrace.AlwaysSample()
	default:
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set as the global provider and install W3C Trace Context propagator.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp, nil
}

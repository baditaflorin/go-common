// Package telemetry provides fleet-wide OpenTelemetry tracing primitives.
//
// Init installs W3C Trace Context propagation and, when an OTLP endpoint is
// configured, a batch-exporting SDK. HTTPMiddleware and NewTransport create
// privacy-conscious HTTP spans. They deliberately omit URLs, headers, bodies,
// SQL text, and baggage from exported data.
package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/secrets"
	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationScope = "github.com/baditaflorin/go-common/telemetry"

const (
	defaultOpenObserveEndpoint = "https://otlp.0exec.com/api/default/v1/traces"
	defaultFleetSecretsURL     = "https://fleet-secrets.0exec.com"
	openObserveTokenSecret     = "openobserve_otlp_ingestion_token"
)

var (
	propagatorOnce sync.Once
	providerMu     sync.Mutex
	sharedProvider *sdktrace.TracerProvider
)

// Config describes one process's tracing configuration. A process should call
// Init once from Go Common's server setup and call Shutdown during termination.
type Config struct {
	ServiceName    string
	ServiceVersion string
	OTLPEndpoint   string
	SampleRate     float64
	Disabled       bool
	provider       *sdktrace.TracerProvider
	otlpHeaders    map[string]string
}

// Option configures tracing initialization.
type Option func(*Config)

// WithOTLP sets the OTLP/HTTP endpoint. HTTPS is required unless the explicit
// insecure option or OTEL_EXPORTER_OTLP_INSECURE=true is set.
func WithOTLP(endpoint string) Option { return func(c *Config) { c.OTLPEndpoint = endpoint } }

// WithSampleRate sets the root trace sampling ratio in [0,1]. The default is
// 0.1; parent-based sampling preserves the decision made by an upstream trace.
func WithSampleRate(rate float64) Option { return func(c *Config) { c.SampleRate = rate } }

// WithDisabled disables trace export while leaving W3C context extraction and
// injection available for upstream/downstream continuity.
func WithDisabled() Option { return func(c *Config) { c.Disabled = true } }

// Init installs W3C Trace Context propagation and initializes the global SDK
// if an endpoint is configured. Services with a dedicated FLEET_SECRETS_API_KEY
// (or legacy FLEET_API_KEY) read the ingestion-only OpenObserve token from Go
// Fleet Secrets and export
// directly over HTTPS. It is safe to call repeatedly from tests and from server
// construction; one process shares one provider/exporter.
func Init(serviceName, serviceVersion string, opts ...Option) *Config {
	propagatorOnce.Do(func() { otel.SetTextMapPropagator(propagation.TraceContext{}) })
	cfg := &Config{ServiceName: serviceName, ServiceVersion: serviceVersion, SampleRate: 0.1}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" && cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = endpoint
	}
	if os.Getenv("OTEL_DISABLED") == "true" {
		cfg.Disabled = true
	}
	if err := configureOpenObserveFromFleetSecrets(cfg, os.Getenv, readOpenObserveToken); err != nil {
		// Export is optional. A vault or collector outage must never prevent
		// a service from starting; without the credential we fail closed and
		// keep W3C propagation active without exporting spans.
		otel.Handle(err)
	}
	if raw := os.Getenv("OTEL_SAMPLE_RATE"); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && n >= 0 && n <= 1 {
			cfg.SampleRate = n
		}
	}
	if cfg.Disabled || cfg.OTLPEndpoint == "" {
		return cfg
	}

	providerMu.Lock()
	defer providerMu.Unlock()
	if sharedProvider != nil {
		cfg.provider = sharedProvider
		return cfg
	}
	tp, err := newProvider(cfg)
	if err != nil {
		otel.Handle(err)
		return cfg
	}
	sharedProvider = tp
	cfg.provider = tp
	return cfg
}

func newProvider(cfg *Config) (*sdktrace.TracerProvider, error) {
	endpoint, err := normalizeEndpoint(cfg.OTLPEndpoint)
	if err != nil {
		return nil, err
	}
	allowInsecure := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true"
	if endpoint.Scheme != "https" && !(allowInsecure && endpoint.Scheme == "http") {
		return nil, errors.New("telemetry: OTLP endpoint must use HTTPS; set OTEL_EXPORTER_OTLP_INSECURE=true only for a protected local network")
	}
	options := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint.String())}
	headers := make(map[string]string, len(cfg.otlpHeaders))
	for key, value := range cfg.otlpHeaders {
		headers[key] = value
	}
	if raw := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); raw != "" {
		overrides, err := parseHeaders(raw)
		if err != nil {
			return nil, fmt.Errorf("telemetry: parse OTEL_EXPORTER_OTLP_HEADERS: %w", err)
		}
		for key, value := range overrides {
			headers[key] = value
		}
	}
	if len(headers) > 0 {
		options = append(options, otlptracehttp.WithHeaders(headers))
	}
	if endpoint.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	caFile := os.Getenv("OTEL_EXPORTER_OTLP_CERTIFICATE")
	clientCert, clientKey := os.Getenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"), os.Getenv("OTEL_EXPORTER_OTLP_CLIENT_KEY")
	if caFile != "" || clientCert != "" || clientKey != "" {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("telemetry: read OTLP CA certificate: %w", err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("telemetry: OTLP CA file contains no certificates")
			}
			tlsConfig.RootCAs = roots
		}
		if clientCert != "" || clientKey != "" {
			if clientCert == "" || clientKey == "" {
				return nil, errors.New("telemetry: both OTLP client certificate and key are required")
			}
			pair, err := tls.LoadX509KeyPair(clientCert, clientKey)
			if err != nil {
				return nil, errors.New("telemetry: failed to load OTLP client certificate/key pair")
			}
			tlsConfig.Certificates = []tls.Certificate{pair}
		}
		options = append(options, otlptracehttp.WithTLSClientConfig(tlsConfig))
	}
	exporter, err := otlptracehttp.New(context.Background(), options...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create OTLP exporter: %w", err)
	}
	sample := cfg.SampleRate
	if sample < 0 || sample > 1 {
		return nil, errors.New("telemetry: sample rate must be between 0 and 1")
	}
	var sampler sdktrace.Sampler
	switch {
	case sample == 0:
		sampler = sdktrace.ParentBased(sdktrace.NeverSample())
	case sample == 1:
		sampler = sdktrace.ParentBased(sdktrace.AlwaysSample())
	default:
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sample))
	}
	res, err := resource.New(context.Background(), resource.WithAttributes(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.ServiceVersion),
	))
	if err != nil {
		_ = exporter.Shutdown(context.Background())
		return nil, fmt.Errorf("telemetry: create resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	return tp, nil
}

// isApprovedOpenObserveEndpoint limits vault-backed OpenObserve credentials
// to the stable fleet hostname and the previous hostname during migration.
func isApprovedOpenObserveEndpoint(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "otlp.0exec.com" || host == "openobserve.0docker.com"
}

// resolveFleetSecretsAPIKey accepts a secret file so deployments can keep the
// bootstrap credential out of Compose env files and container environment.
// Secret files must be regular files with no group/world permissions.
func resolveFleetSecretsAPIKey(getenv func(string) string) (string, error) {
	if key := getenv("FLEET_SECRETS_API_KEY"); key != "" {
		return key, nil
	}
	if path := strings.TrimSpace(getenv("FLEET_SECRETS_API_KEY_FILE")); path != "" {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("telemetry: Fleet Secrets API key file is unavailable or has unsafe permissions")
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("telemetry: Fleet Secrets API key file could not be read")
		}
		key := strings.TrimSpace(string(value))
		if key == "" {
			return "", errors.New("telemetry: Fleet Secrets API key file is empty")
		}
		return key, nil
	}
	return getenv("FLEET_API_KEY"), nil
}

// configureOpenObserveFromFleetSecrets enables direct OTLP export when a
// service has a dedicated FLEET_SECRETS_API_KEY (preferred), a protected
// FLEET_SECRETS_API_KEY_FILE, or a legacy FLEET_API_KEY. The token is ingestion-only and is fetched over verified HTTPS
// from the per-service allowlisted vault identity.
// Explicit endpoints take precedence over the default URL. If a fleet secret
// API key is present, its allowlisted ingestion token authenticates that
// endpoint unless explicit OTLP headers are supplied. Explicit OTLP headers
// are applied by newProvider and override the generated Authorization header.
func configureOpenObserveFromFleetSecrets(cfg *Config, getenv func(string) string, readToken func(context.Context, string, string) (string, error)) error {
	if cfg == nil || cfg.Disabled || getenv("OTEL_EXPORTER_OTLP_HEADERS") != "" {
		return nil
	}
	if cfg.OTLPEndpoint != "" && !isApprovedOpenObserveEndpoint(cfg.OTLPEndpoint) {
		// Never send an OpenObserve ingestion credential to an arbitrary OTLP host.
		return nil
	}
	apiKey, err := resolveFleetSecretsAPIKey(getenv)
	if err != nil {
		return err
	}
	if apiKey == "" {
		return nil
	}
	secretsURL := getenv("FLEET_SECRETS_URL")
	if secretsURL == "" {
		secretsURL = defaultFleetSecretsURL
	}
	parsed, err := url.Parse(strings.TrimSpace(secretsURL))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "fleet-secrets.0exec.com") || (parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("telemetry: fleet secrets URL must use the approved HTTPS host without credentials or query data")
	}
	if readToken == nil {
		return errors.New("telemetry: fleet secrets reader is unavailable")
	}
	token, err := readToken(context.Background(), secretsURL, apiKey)
	if err != nil || token == "" {
		return errors.New("telemetry: could not load OpenObserve ingestion credential from fleet secrets")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("default:" + token))
	if cfg.OTLPEndpoint == "" {
		cfg.OTLPEndpoint = defaultOpenObserveEndpoint
	}
	cfg.otlpHeaders = map[string]string{
		"Authorization": "Basic " + encoded,
		"stream-name":   "default",
	}
	return nil
}

func readOpenObserveToken(ctx context.Context, baseURL, apiKey string) (string, error) {
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Never forward the vault API key across a redirect.
			return http.ErrUseLastResponse
		},
	}
	return secrets.New(baseURL, apiKey, client).Get(ctx, openObserveTokenSecret)
}

func normalizeEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Error includes the original input. Keep credentials and query
		// material out of startup diagnostics by returning a fixed message.
		return nil, errors.New("telemetry: invalid OTLP endpoint URL")
	}
	if endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("telemetry: OTLP endpoint must include scheme and host")
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, errors.New("telemetry: OTLP endpoint scheme must be https or http")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("telemetry: credentials and query data are not allowed in the OTLP endpoint URL; use headers")
	}
	return endpoint, nil
}

func parseHeaders(raw string) (map[string]string, error) {
	values := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, errors.New("expected comma-separated key=value pairs")
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			return nil, errors.New("invalid URL-escaped header value")
		}
		values[strings.TrimSpace(key)] = decoded
	}
	return values, nil
}

// Shutdown flushes exported spans. The caller supplies a bounded context.
func (c *Config) Shutdown(ctx context.Context) error {
	if c == nil || c.provider == nil {
		return nil
	}
	return c.provider.Shutdown(ctx)
}

// Tracer returns a named OpenTelemetry tracer for application-level spans.
func Tracer(scope string) trace.Tracer { return otel.Tracer(scope) }

// StartSpan starts an application span. Keep operation names and attributes
// low-cardinality and free of user data, URLs, SQL text, and secrets.
func StartSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer(instrumentationScope).Start(ctx, operation, trace.WithAttributes(attrs...))
}

// HTTPMiddleware extracts W3C trace context and creates a server span without
// recording request paths, query strings, headers, or bodies. Health and
// metrics endpoints are excluded to avoid filling the trace store with probes.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Tracer(instrumentationScope).Start(ctx, "HTTP "+r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)),
		)
		metrics := httpsnoop.CaptureMetrics(next, w, r.WithContext(ctx))
		if metrics.Code > 0 {
			span.SetAttributes(attribute.Int("http.response.status_code", metrics.Code))
			if metrics.Code >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, "server error")
			}
		}
		span.End()
	})
}

// NewTransport adds an outbound client span and injects W3C trace context.
// It records only method, response status, and transport-error type; the URL,
// query string, request/response headers, and bodies are intentionally omitted.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &roundTripper{base: base}
}

type roundTripper struct{ base http.RoundTripper }

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := Tracer(instrumentationScope).Start(req.Context(), "HTTP "+req.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", req.Method)),
	)
	defer span.End()
	clone := req.Clone(ctx)
	clone.Header = req.Header.Clone()
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

func isProbe(path string) bool {
	switch path {
	case "/health", "/healthz", "/readyz", "/livez", "/metrics", "/metrics/json", "/version", "/selftest":
		return true
	default:
		return false
	}
}

// ExporterTimeout is the default bound used by service shutdown hooks.
const ExporterTimeout = 5 * time.Second

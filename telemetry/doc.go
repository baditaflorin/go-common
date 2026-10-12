// Package telemetry installs W3C trace propagation, creates privacy-conscious
// inbound/outbound HTTP spans, and exports them with OTLP/HTTP when configured.
//
// Go Common's server.New initializes this package automatically. Applications
// using a custom server may call Init and wrap the handler with HTTPMiddleware.
// safehttp.NewClient adds NewTransport automatically.
//
// For automatic OpenObserve export, provide a service-scoped
// FLEET_SECRETS_API_KEY (recommended) or legacy FLEET_API_KEY that can read the
// allowlisted ingestion secret from Go Fleet Secrets. Without an endpoint
// override, export uses https://openobserve.0own.com/api/default/v1/traces.
// An OTEL_EXPORTER_OTLP_ENDPOINT override is preserved; the vault token is
// attached only for the approved HTTPS hosts otlp.0exec.com and
// openobserve.0own.com. Other collector hosts must use
// OTEL_EXPORTER_OTLP_HEADERS for their own credentials.
// Header values follow the OpenTelemetry comma-separated, URL-escaped key=value
// convention.
// OTEL_EXPORTER_OTLP_CERTIFICATE may specify a private CA PEM; client
// certificates can be supplied with OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE and
// OTEL_EXPORTER_OTLP_CLIENT_KEY. OTEL_SAMPLE_RATE defaults to 0.1 and uses
// parent-based sampling. OTEL_DISABLED=true turns off exporting. Cleartext
// HTTP is rejected unless OTEL_EXPORTER_OTLP_INSECURE=true is explicitly set
// for a protected network. Trace context propagation uses W3C traceparent only;
// baggage is not propagated by default.
//
// The ingestion credential is read once, before Init returns, with a 2 s limit.
// If that read fails transiently (a timeout, a connection error, or a 5xx, 408
// or 429 answer), Init still returns at once and a bounded background retry
// installs the tracer provider when the credential is finally read: at most six
// more attempts after waits of 2, 4, 8, 16, 32 and 64 s (about two minutes, each
// wait shortened by up to 20% so that services restarting together do not meet
// the vault in lockstep), each with the same 2 s limit. A 401, 403 or 404, and
// any other answer the vault gave on purpose, is a configuration error: it is
// reported once and never retried, and tracing stays off. Config.Shutdown
// cancels the retry. One line is logged if tracing comes up late and one error
// line if the attempts run out; neither contains a credential.
//
// HTTP spans intentionally omit URLs, query strings, headers, and bodies.
// Application-created spans should also use low-cardinality names and avoid
// user data, credentials, raw SQL, and other secrets in attributes.
package telemetry

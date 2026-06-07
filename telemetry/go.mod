// telemetry is a nested module so the heavy OpenTelemetry + gRPC + protobuf
// + genproto dependency tree (~138 packages, ~8 MB of binary) lives ONLY
// here instead of in the root github.com/baditaflorin/go-common module.
//
// Before this split every go-common consumer (~438 fleet services) pulled
// the entire OTel SDK into its build graph / go.sum / module cache even
// though a fleet-wide audit found ZERO services import this package. Keeping
// telemetry in its own module means root go-common no longer requires OTel,
// so those services drop the whole tree on their next `go mod tidy` with no
// code change. The handful of future callers that want OTel tracing add one
// require: github.com/baditaflorin/go-common/telemetry.
//
// The replace directive below lets in-repo builds/tests resolve the parent
// against local source; it is ignored by external consumers (replace only
// applies to the main module), who pick up the pinned go-common version.
module github.com/baditaflorin/go-common/telemetry

go 1.25.0

require (
	github.com/baditaflorin/go-common v0.60.0
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/baditaflorin/go-common => ../

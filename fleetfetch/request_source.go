package fleetfetch

import (
	"context"
	"fmt"
	"strings"
)

type requestSourceContextKey struct{}

// WithRequestSource attaches a per-inbound-request fetch source to ctx.
// The server package uses this after validating the source query parameter.
func WithRequestSource(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, requestSourceContextKey{}, source)
}

// RequestSourceFromContext returns a request-scoped fetch source when one is
// present. SourceLive is the empty string, so the bool distinguishes an
// explicit live override from the absence of a request override.
func RequestSourceFromContext(ctx context.Context) (Source, bool) {
	if ctx == nil {
		return SourceLive, false
	}
	source, ok := ctx.Value(requestSourceContextKey{}).(Source)
	return source, ok
}

// ParseSource validates a source name accepted by the source query parameter.
func ParseSource(value string) (Source, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "live":
		return SourceLive, nil
	case "commoncrawl":
		return SourceCommonCrawl, nil
	default:
		return SourceLive, fmt.Errorf("fleetfetch: unsupported source %q", value)
	}
}

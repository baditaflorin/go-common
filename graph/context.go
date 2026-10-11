package graph

import (
	"context"
	"fmt"
	"strings"
)

type targetContextKey struct{}

// WithTarget annotates an outbound request context with the canonical Fleet
// Graph service ID for its destination. Use it when a catalogued service is
// reached through a shared IP or another address that cannot identify the
// logical service. It changes telemetry only; it never changes routing.
func WithTarget(ctx context.Context, serviceID string) (context.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("graph target: context is required")
	}
	serviceID = strings.ToLower(strings.TrimSpace(serviceID))
	if !validGraphLabel(serviceID) {
		return nil, fmt.Errorf("graph target: invalid service ID")
	}
	return context.WithValue(ctx, targetContextKey{}, serviceID), nil
}

func targetFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	target, _ := ctx.Value(targetContextKey{}).(string)
	return target
}

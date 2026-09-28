package graph

import (
	"context"
	"strings"
)

type authenticatedCallerKey struct{}

// WithAuthenticatedCaller stores a caller identity established by an
// in-process authentication middleware. Never call this with a value copied
// directly from an HTTP request header.
func WithAuthenticatedCaller(ctx context.Context, principal string) context.Context {
	principal = serviceCallerID(principal)
	if principal == "" {
		return ctx
	}
	return context.WithValue(ctx, authenticatedCallerKey{}, principal)
}

// AuthenticatedCallerFromContext returns a caller identity previously set by
// WithAuthenticatedCaller. It is empty for unauthenticated, generic, or
// non-service principals.
func AuthenticatedCallerFromContext(ctx context.Context) string {
	return authenticatedCaller(ctx)
}

func authenticatedCaller(ctx context.Context) string {
	caller, _ := ctx.Value(authenticatedCallerKey{}).(string)
	return caller
}

// serviceCallerID accepts only the fleet service-ID shapes. Generic gateway
// identities such as "internal" and ordinary user principals stay unknown in
// the service topology.
func serviceCallerID(principal string) string {
	id := strings.TrimSpace(principal)
	if id == "" || id != strings.ToLower(id) || strings.ContainsAny(id, "/:@ \\?&") {
		return ""
	}
	if !strings.HasPrefix(id, "go_") && !strings.Contains(id, "-") {
		return ""
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return ""
	}
	return id
}

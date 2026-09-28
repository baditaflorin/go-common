package graphidentity

import (
	"context"
	"strings"
)

type callerKey struct{}

// WithVerifiedPrincipal stores a service-shaped identity that an in-process
// auth middleware has already verified. This package is internal so service
// applications cannot mark request-header claims as authenticated callers.
func WithVerifiedPrincipal(ctx context.Context, principal string) context.Context {
	principal = serviceCallerID(principal)
	if principal == "" {
		return ctx
	}
	return context.WithValue(ctx, callerKey{}, principal)
}

// VerifiedPrincipal returns a previously stored verified service identity.
func VerifiedPrincipal(ctx context.Context) string {
	principal, _ := ctx.Value(callerKey{}).(string)
	return principal
}

// NormalizeServiceCallerID validates the shape of a service identity before
// graph middleware accepts it as a caller label.
func NormalizeServiceCallerID(principal string) string {
	return serviceCallerID(principal)
}

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

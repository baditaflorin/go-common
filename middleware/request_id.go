package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/baditaflorin/go-common/header"
)

type contextKey string

const RequestIDKey contextKey = "request_id"

// RequestID generates a unique ID for each request if not present
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(header.RequestID)
		if reqID == "" {
			reqID = generateID()
			r.Header.Set(header.RequestID, reqID)
		}

		// Inject into context
		ctx := context.WithValue(r.Context(), RequestIDKey, reqID)

		// Set response header
		w.Header().Set(header.RequestID, reqID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GetRequestID helper to extract ID from context
func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(RequestIDKey).(string); ok {
		return v
	}
	return ""
}

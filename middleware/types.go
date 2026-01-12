package middleware

import "net/http"

// Middleware is a standard function that wraps an http.Handler
type Middleware func(http.Handler) http.Handler

// Chain applies a list of middlewares to a handler
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

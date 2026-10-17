package degraded

import "context"

type ctxKey struct{}

// NewContext returns a context that carries s, so a library called with that
// context (a long-lived client, say) can record degradation against the request
// it is serving instead of needing the Sink passed to it.
func NewContext(ctx context.Context, s *Sink) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the Sink NewContext stored, or nil. A nil *Sink is safe to
// use: Append on it does nothing.
func FromContext(ctx context.Context) *Sink {
	s, _ := ctx.Value(ctxKey{}).(*Sink)
	return s
}

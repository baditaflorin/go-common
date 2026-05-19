// Package degraded accumulates degradation reasons for the current
// request and surfaces them in the response.Envelope "degraded" array.
//
// Services call degraded.Add(ctx, reason) when a soft dependency fails;
// the envelope includes those reasons so clients and monitoring systems
// can distinguish a degraded response from a hard failure.
package degraded

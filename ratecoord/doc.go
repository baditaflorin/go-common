// Package ratecoord provides a distributed rate-limit coordination client.
// Services consult the coordinator before sending requests to rate-limited
// upstreams, sharing a global token bucket across fleet instances.
package ratecoord

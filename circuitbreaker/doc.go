// Package circuitbreaker implements a per-host circuit breaker state machine
// (ok → backing_off → failed → ok) that protects services from cascading
// failures when upstream dependencies are overloaded or down.
package circuitbreaker

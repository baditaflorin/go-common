// Package promx provides fleet-wide Prometheus collectors and a single-call
// AutoWire function that installs all observers.
//
// Usage:
//
//	egressColl, httpColl, authColl := promx.AutoWire(serviceName, version)
//	safehttp.SetDefaultObserver(egressColl)
//
// Prefer telemetry.Init which calls AutoWire internally.
package promx

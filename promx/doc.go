// Package promx provides fleet-wide Prometheus collectors and a single-call
// AutoWire function that installs all observers.
//
// Usage:
//
//	egressColl, httpColl, authColl := promx.AutoWire(serviceName, version)
//	safehttp.SetDefaultObserver(egressColl)
//
// Prefer server.New, which calls AutoWire and installs the egress observer
// itself. telemetry.Init does not call AutoWire.
package promx

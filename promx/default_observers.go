package promx

import (
	"github.com/baditaflorin/go-common/backoffcoord"
	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/degraded"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/response"
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/workpool"
)

// These tiny helpers exist so the AutoWire body in promx.go does not
// have to grow an import for every new observer-sink package.
// Splitting them out also makes the cross-package wiring grep-able.

func setEnvelopeDefaultObserver(c *EnvelopeCollectors)         { response.SetDefaultObserver(c) }
func setDegradedDefaultObserver(c *DegradedCollectors)         { degraded.SetDefaultObserver(c) }
func setSafehttpBackoffDefaultObserver(c *BackoffCollectors)   { safehttp.SetDefaultBackoffObserver(c) }
func setFleetFetchDefaultObserver(c *FleetFetchCollectors)     { fleetfetch.SetDefaultObserver(c) }
func setCircuitDefaultObserver(c *CircuitCollectors)           { circuitbreaker.SetDefaultObserver(c) }
func setWorkpoolDefaultObserver(c *WorkpoolCollectors)         { workpool.SetDefaultObserver(c) }
func setLoadshedDefaultObserver(c *LoadshedCollectors)         { loadshed.SetDefaultObserver(c) }
func setBackoffCoordDefaultObserver(c *BackoffCoordCollectors) { backoffcoord.SetDefaultObserver(c) }

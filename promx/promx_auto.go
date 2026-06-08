package promx

// AutoWire performs the full bootstrap go-common/server calls at startup:
// Init the shared registry, then create (or reuse) one each of the egress,
// inbound-HTTP, and auth collector sets. Returned collectors are package
// singletons — repeated AutoWire calls (e.g. across tests) return the same
// instances without re-registering with Prometheus.
//
// Callers outside go-common/server can use AutoWire to get the same
// canonical wiring if they don't go through server.New (e.g. a service
// that uses its own router and wants a one-line metrics setup).
func AutoWire(serviceID, version string) (*EgressCollectors, *HTTPCollectors, *AuthCollectors) {
	reg := Init(serviceID, version)
	autoMu.Lock()
	defer autoMu.Unlock()
	// Rebind to the current registry if a prior AutoWire was bound to
	// a different one (Init re-ran with new identity, typically in
	// tests). MustRegister-on-fresh-registry is safe; the old
	// collectors are GC'd along with the old registry.
	if autoBoundReg != reg {
		autoEgress = nil
		autoHTTP = nil
		autoAuth = nil
		autoSelftest = nil
		autoDep = nil
		autoRateCoord = nil
		autoPolicy = nil
		autoEnvelope = nil
		autoDegraded = nil
		autoAdmin = nil
		autoBackoff = nil
		autoFleetFetch = nil
		autoCircuit = nil
		autoWorkpool = nil
		autoLoadshed = nil
		autoBackoffCoord = nil
		autoBoundReg = reg
	}
	if autoEgress == nil {
		autoEgress = NewEgressCollectors(reg)
	}
	if autoHTTP == nil {
		autoHTTP = NewHTTPCollectors(reg)
	}
	if autoAuth == nil {
		autoAuth = NewAuthCollectors(reg)
	}
	if autoSelftest == nil {
		autoSelftest = NewSelftestCollectors(reg)
	}
	if autoDep == nil {
		autoDep = NewDepCollectors(reg)
	}
	if autoRateCoord == nil {
		autoRateCoord = NewRateCoordCollectors(reg)
	}
	if autoPolicy == nil {
		autoPolicy = NewPolicyCollectors(reg)
		// policyeval has a process-wide default observer because
		// policyeval.Evaluate is a free function (not a method on
		// some Client we can SetObserver on). Wire the singleton
		// here so any Evaluate / EvaluateLabeled call in the
		// process produces metrics without per-call ceremony.
		setPolicyDefaultObserver(autoPolicy)
	}
	if autoEnvelope == nil {
		autoEnvelope = NewEnvelopeCollectors(reg)
		setEnvelopeDefaultObserver(autoEnvelope)
	}
	if autoDegraded == nil {
		autoDegraded = NewDegradedCollectors(reg)
		setDegradedDefaultObserver(autoDegraded)
	}
	if autoAdmin == nil {
		autoAdmin = NewAdminCollectors(reg)
	}
	if autoBackoff == nil {
		autoBackoff = NewBackoffCollectors(reg)
		setSafehttpBackoffDefaultObserver(autoBackoff)
	}
	if autoFleetFetch == nil {
		autoFleetFetch = NewFleetFetchCollectors(reg)
		setFleetFetchDefaultObserver(autoFleetFetch)
	}
	if autoCircuit == nil {
		autoCircuit = NewCircuitCollectors(reg)
		setCircuitDefaultObserver(autoCircuit)
	}
	if autoWorkpool == nil {
		autoWorkpool = NewWorkpoolCollectors(reg)
		setWorkpoolDefaultObserver(autoWorkpool)
	}
	if autoLoadshed == nil {
		autoLoadshed = NewLoadshedCollectors(reg)
		setLoadshedDefaultObserver(autoLoadshed)
	}
	if autoBackoffCoord == nil {
		autoBackoffCoord = NewBackoffCoordCollectors(reg)
		setBackoffCoordDefaultObserver(autoBackoffCoord)
	}
	return autoEgress, autoHTTP, autoAuth
}

// AutoEnvelope returns the singleton EnvelopeCollectors.
func AutoEnvelope() *EnvelopeCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoEnvelope
}

// AutoDegraded returns the singleton DegradedCollectors.
func AutoDegraded() *DegradedCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoDegraded
}

// AutoAdmin returns the singleton AdminCollectors for apikey admin
// calls. Attach via Client.AdminObs = promx.AutoAdmin().
func AutoAdmin() *AdminCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoAdmin
}

// AutoBackoff returns the singleton safehttp BackoffCollectors.
func AutoBackoff() *BackoffCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoBackoff
}

// AutoFleetFetch returns the singleton FleetFetchCollectors.
func AutoFleetFetch() *FleetFetchCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoFleetFetch
}

// AutoCircuit returns the singleton CircuitCollectors.
func AutoCircuit() *CircuitCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoCircuit
}

// AutoWorkpool returns the singleton WorkpoolCollectors.
func AutoWorkpool() *WorkpoolCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoWorkpool
}

// AutoLoadshed returns the singleton LoadshedCollectors. AutoWire has
// already installed it as the process-wide loadshed.Observer, so every
// loadshed.Gate emits metrics without further wiring.
func AutoLoadshed() *LoadshedCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoLoadshed
}

// AutoBackoffCoord returns the singleton BackoffCoordCollectors for
// the backoffcoord.Client package (the standalone consult client, not
// safehttp's transport-level hook).
func AutoBackoffCoord() *BackoffCoordCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoBackoffCoord
}

// AutoSelftest returns the singleton SelftestCollectors created by
// AutoWire. Returns nil if AutoWire has not been called. Wire it on
// your selftest.Suite via selftest.WithObserver(promx.AutoSelftest()).
func AutoSelftest() *SelftestCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoSelftest
}

// AutoDep returns the singleton DepCollectors created by AutoWire.
// Returns nil if AutoWire has not been called. Wire on a depcheck
// registry via deps.SetObserver(promx.AutoDep()).
func AutoDep() *DepCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoDep
}

// AutoRateCoord returns the singleton RateCoordCollectors created by
// AutoWire. Returns nil if AutoWire has not been called. Wire on a
// ratecoord.Client via client.SetObserver(promx.AutoRateCoord()).
func AutoRateCoord() *RateCoordCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoRateCoord
}

// AutoPolicy returns the singleton PolicyCollectors created by
// AutoWire. Returns nil if AutoWire has not been called. AutoWire
// has already called policyeval.SetDefaultObserver with this
// instance, so callers do not need to wire it further.
func AutoPolicy() *PolicyCollectors {
	autoMu.Lock()
	defer autoMu.Unlock()
	return autoPolicy
}

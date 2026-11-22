package proxysupplier

import "time"

// ASNCapacity says how much of a Webshare catalog can serve a request right
// now, counted by network rather than by IP, so that a caller can tell "some
// ASNs are quarantined but the others still serve at full speed" from "there
// is nothing left to try".
type ASNCapacity struct {
	// HealthyASNs is the number of ASNs that are not quarantined and still
	// have at least one eligible IP. An ASN that has never been tried (no
	// health samples yet) counts as healthy. IPs without a known ASN count as
	// one more ASN.
	HealthyASNs int `json:"healthy_asns"`
	// EligibleIPs is the number of IPs in those ASNs that are eligible: not
	// in their per-IP cooldown and not in a /24 that is cooling down.
	EligibleIPs int `json:"eligible_ips"`
	// QuarantinedASNs is the number of quarantined ASNs in the catalog.
	QuarantinedASNs int `json:"quarantined_asns"`
	// ProbeReadyASNs is the number of quarantined ASNs whose cooldown and probe
	// interval have elapsed so a single recovery probe may run now.
	ProbeReadyASNs int `json:"probe_ready_asns,omitempty"`
	// NextProbeIn is the shortest time until a quarantined ASN may send its
	// next probe (its cooldown end, or its probe interval after the last
	// probe). Zero when a probe is due now or no ASN is quarantined (see
	// QuarantinedASNs).
	NextProbeIn time.Duration `json:"next_probe_in"`
}

// WebshareCapacityReporter is implemented by the webshare_multi supplier, in
// the same optional-capability style as WebshareASNExcludedSelector.
// HealthyCapacity covers one named pool (zero for an unknown pool ID);
// HealthyCapacityAll covers every configured pool, of every class. Both read
// the shared health tracker and do not change any state.
type WebshareCapacityReporter interface {
	HealthyCapacity(poolID string) ASNCapacity
	HealthyCapacityAll() ASNCapacity
}

// HealthyCapacity implements WebshareCapacityReporter for one named pool.
func (s *webshareMultiSupplier) HealthyCapacity(poolID string) ASNCapacity {
	if s == nil {
		return ASNCapacity{}
	}
	for _, p := range s.pools {
		if p.ID == poolID {
			return s.networkHealth.capacity(p.supplier.snapshotList(), time.Now())
		}
	}
	return ASNCapacity{}
}

// HealthyCapacityAll implements WebshareCapacityReporter for every pool.
func (s *webshareMultiSupplier) HealthyCapacityAll() ASNCapacity {
	if s == nil {
		return ASNCapacity{}
	}
	var entries []*webshareDirectEntry
	for _, p := range s.pools {
		entries = append(entries, p.supplier.snapshotList()...)
	}
	return s.networkHealth.capacity(entries, time.Now())
}

func (s *webshareDirectSupplier) snapshotList() []*webshareDirectEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.list
}

// capacity counts entries by ASN health at now. A nil tracker treats every
// ASN as healthy.
func (h *webshareNetworkHealth) capacity(entries []*webshareDirectEntry, now time.Time) ASNCapacity {
	nowNano := now.UnixNano()
	var c ASNCapacity
	healthy := make(map[int]struct{})
	quarantined := make(map[int]struct{})
	nextProbe := int64(-1)

	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		asn := pacingKey(e.asnNumber)
		if h != nil && asn > 0 {
			if ah, ok := h.asns[asn]; ok && ah.quarantined {
				if _, seen := quarantined[asn]; !seen {
					quarantined[asn] = struct{}{}
					at := ah.cooldownUntil
					if ah.lastProbeNano != 0 {
						at = max(at, ah.lastProbeNano+webshareASNProbeInterval.Nanoseconds())
					}
					wait := max(at-nowNano, 0)
					if wait == 0 {
						c.ProbeReadyASNs++
					}
					if nextProbe < 0 || wait < nextProbe {
						nextProbe = wait
					}
				}
				continue
			}
		}
		if !e.eligible(nowNano) {
			continue
		}
		if h != nil && e.subnet24 != "" {
			if sh, ok := h.subnets[e.subnet24]; ok && sh.cooldownUntil > nowNano {
				continue
			}
		}
		healthy[asn] = struct{}{}
		c.EligibleIPs++
	}
	c.HealthyASNs = len(healthy)
	c.QuarantinedASNs = len(quarantined)
	if nextProbe > 0 {
		c.NextProbeIn = time.Duration(nextProbe)
	}
	return c
}

var _ WebshareCapacityReporter = (*webshareMultiSupplier)(nil)

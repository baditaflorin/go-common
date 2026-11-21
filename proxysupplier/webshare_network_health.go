package proxysupplier

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Tiered /24 subnet and ASN health tuning. Vars so tests can shrink durations.
var (
	webshareSubnetWindow           = 2 * time.Minute
	webshareSubnetFailureThreshold = 2
	webshareSubnetCooldown         = 10 * time.Minute

	webshareASNWindowSize       = 40
	webshareASNMinSamples       = 10
	webshareASNDegradedRatio    = 0.25
	webshareASNHeavyRatio       = 0.40
	webshareASNQuarantineRatio  = 0.50
	webshareASNBaseCooldown     = 10 * time.Minute
	webshareASNMaxCooldown      = 30 * time.Minute
	webshareASNProbeInterval    = 30 * time.Second
	webshareASNCloseAfterProbes = 2
)

const (
	webshareMaxTrackedSubnets = 4096
	webshareMaxTrackedASNs    = 512

	// WebshareASNStateHealthy indicates normal selection weight (100%).
	WebshareASNStateHealthy = "healthy"
	// WebshareASNStateDegraded indicates elevated failures with thinned weight.
	WebshareASNStateDegraded = "degraded"
	// WebshareASNStateQuarantined indicates the ASN is cooling down with periodic probes.
	WebshareASNStateQuarantined = "quarantined"
)

// WebshareASNStatus is the safe, non-secret health summary for one ASN.
type WebshareASNStatus struct {
	ASNNumber                int     `json:"asn_number"`
	ASNName                  string  `json:"asn_name,omitempty"`
	IPs                      int     `json:"ips"`
	Subnets24                int     `json:"subnets_24"`
	CooledSubnets24          int     `json:"cooled_subnets_24"`
	State                    string  `json:"state"`
	FailureRatio             float64 `json:"failure_ratio"`
	Samples                  int     `json:"samples"`
	OKTotal                  uint64  `json:"ok_total"`
	FailTotal                uint64  `json:"fail_total"`
	CooldownRemainingSeconds int     `json:"cooldown_remaining_seconds"`
	TripsTotal               uint64  `json:"trips_total"`
}

// WebshareNetworkSnapshot summarizes ASN and /24 subnet health across one or
// more Webshare pools without exposing individual proxy IPs or credentials.
type WebshareNetworkSnapshot struct {
	TotalASNs       int                 `json:"total_asns"`
	HealthyASNs     int                 `json:"healthy_asns"`
	DegradedASNs    int                 `json:"degraded_asns"`
	QuarantinedASNs int                 `json:"quarantined_asns"`
	TotalSubnets24  int                 `json:"total_subnets_24"`
	CooledSubnets24 int                 `json:"cooled_subnets_24"`
	ASNs            []WebshareASNStatus `json:"asns,omitempty"`
}

// WebshareNetworkHealthProvider is implemented by suppliers that track per-ASN
// and per-/24 subnet health.
type WebshareNetworkHealthProvider interface {
	WebshareNetworkHealth() WebshareNetworkSnapshot
}

type subnetHealth struct {
	recentFails   map[string]int64 // addr -> unixNano of recent failure
	cooldownUntil int64            // unixNano; 0 = not cooling
}

type asnHealth struct {
	name             string
	outcomes         []bool // ring buffer: true=ok, false=fail
	pos              int
	count            int
	okTotal          uint64
	failTotal        uint64
	quarantined      bool
	cooldownUntil    int64
	lastProbeNano    int64
	probeSuccesses   int
	consecutiveTrips int
	tripsTotal       uint64
}

func (a *asnHealth) recordOutcome(ok bool) {
	win := webshareASNWindowSize
	if win <= 0 {
		win = 40
	}
	if len(a.outcomes) != win {
		a.outcomes = make([]bool, win)
		a.pos = 0
		a.count = 0
	}
	a.outcomes[a.pos] = ok
	a.pos = (a.pos + 1) % win
	if a.count < win {
		a.count++
	}
}

func (a *asnHealth) failureStats() (samples int, ratio float64) {
	if a.count == 0 {
		return 0, 0
	}
	fails := 0
	for i := 0; i < a.count; i++ {
		if !a.outcomes[i] {
			fails++
		}
	}
	return a.count, float64(fails) / float64(a.count)
}

func (a *asnHealth) resetWindow() {
	a.pos = 0
	a.count = 0
}

func (a *asnHealth) stateString(nowNano int64) (state string, ratio float64, samples int, cooldownSec int) {
	samples, ratio = a.failureStats()
	if a.quarantined {
		rem := int((a.cooldownUntil - nowNano) / int64(time.Second))
		if rem < 0 {
			rem = 0
		}
		return WebshareASNStateQuarantined, ratio, samples, rem
	}
	if samples >= webshareASNMinSamples && ratio >= webshareASNDegradedRatio {
		return WebshareASNStateDegraded, ratio, samples, 0
	}
	return WebshareASNStateHealthy, ratio, samples, 0
}

// webshareNetworkHealth tracks /24 subnet cooldowns and per-ASN adaptive
// thinning/quarantine, safely shared across pools in webshareMultiSupplier.
type webshareNetworkHealth struct {
	mu      sync.Mutex
	subnets map[string]*subnetHealth
	asns    map[int]*asnHealth
}

func newWebshareNetworkHealth() *webshareNetworkHealth {
	return &webshareNetworkHealth{
		subnets: make(map[string]*subnetHealth),
		asns:    make(map[int]*asnHealth),
	}
}

// Record feeds a single exit outcome into the /24 subnet and ASN trackers.
func (h *webshareNetworkHealth) Record(asnNumber int, asnName, subnet24, addr string, ok bool, now time.Time) {
	if h == nil {
		return
	}
	nowNano := now.UnixNano()
	h.mu.Lock()
	defer h.mu.Unlock()

	if subnet24 != "" {
		h.recordSubnetLocked(subnet24, addr, ok, nowNano)
	}
	if asnNumber > 0 {
		h.recordASNLocked(asnNumber, asnName, ok, nowNano)
	}
}

func (h *webshareNetworkHealth) recordSubnetLocked(subnet24, addr string, ok bool, nowNano int64) {
	sh, exists := h.subnets[subnet24]
	if !exists {
		if ok || len(h.subnets) >= webshareMaxTrackedSubnets {
			return
		}
		sh = &subnetHealth{recentFails: make(map[string]int64)}
		h.subnets[subnet24] = sh
	}
	if ok {
		delete(sh.recentFails, addr)
		if sh.cooldownUntil > 0 && nowNano >= sh.cooldownUntil {
			sh.cooldownUntil = 0
		}
		return
	}
	cutoff := nowNano - webshareSubnetWindow.Nanoseconds()
	for k, ts := range sh.recentFails {
		if ts < cutoff {
			delete(sh.recentFails, k)
		}
	}
	if addr != "" {
		sh.recentFails[addr] = nowNano
	}
	if len(sh.recentFails) >= webshareSubnetFailureThreshold {
		sh.cooldownUntil = nowNano + webshareSubnetCooldown.Nanoseconds()
	}
}

func (h *webshareNetworkHealth) recordASNLocked(asnNumber int, asnName string, ok bool, nowNano int64) {
	ah, exists := h.asns[asnNumber]
	if !exists {
		if len(h.asns) >= webshareMaxTrackedASNs {
			return
		}
		ah = &asnHealth{name: strings.TrimSpace(asnName)}
		h.asns[asnNumber] = ah
	} else if ah.name == "" && asnName != "" {
		ah.name = strings.TrimSpace(asnName)
	}
	if ok {
		ah.okTotal++
	} else {
		ah.failTotal++
	}

	if ah.quarantined {
		if nowNano < ah.cooldownUntil {
			return
		}
		if ok {
			ah.probeSuccesses++
			if ah.probeSuccesses >= webshareASNCloseAfterProbes {
				ah.quarantined = false
				ah.cooldownUntil = 0
				ah.probeSuccesses = 0
				ah.consecutiveTrips = 0
				ah.resetWindow()
				ah.recordOutcome(true)
			}
		} else {
			ah.probeSuccesses = 0
			ah.tripLocked(nowNano)
		}
		return
	}

	ah.recordOutcome(ok)
	samples, ratio := ah.failureStats()
	if !ok && samples >= webshareASNMinSamples && ratio >= webshareASNQuarantineRatio {
		ah.tripLocked(nowNano)
	}
}

func (a *asnHealth) tripLocked(nowNano int64) {
	a.quarantined = true
	a.probeSuccesses = 0
	a.consecutiveTrips++
	a.tripsTotal++
	cd := webshareASNBaseCooldown * time.Duration(a.consecutiveTrips)
	if cd > webshareASNMaxCooldown {
		cd = webshareASNMaxCooldown
	}
	a.cooldownUntil = nowNano + cd.Nanoseconds()
	a.lastProbeNano = 0
	a.resetWindow()
}

// Allows reports whether an entry in (asnNumber, subnet24) should be selected
// on normal pass 1. Degraded ASNs are deterministically thinned by cursor so
// they receive 50% or 25% of their normal traffic; quarantined ASNs only allow
// a single probe per webshareASNProbeInterval after cooldown expires.
func (h *webshareNetworkHealth) Allows(asnNumber int, subnet24 string, nowNano int64, cursor uint64) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if subnet24 != "" {
		if sh, ok := h.subnets[subnet24]; ok && sh.cooldownUntil > nowNano {
			return false
		}
	}
	if asnNumber <= 0 {
		return true
	}
	ah, ok := h.asns[asnNumber]
	if !ok {
		return true
	}
	if ah.quarantined {
		if nowNano < ah.cooldownUntil {
			return false
		}
		if ah.lastProbeNano != 0 && nowNano-ah.lastProbeNano < webshareASNProbeInterval.Nanoseconds() {
			return false
		}
		ah.lastProbeNano = nowNano
		return true
	}
	samples, ratio := ah.failureStats()
	if samples >= webshareASNMinSamples {
		if ratio >= webshareASNHeavyRatio {
			return cursor%4 == 0
		}
		if ratio >= webshareASNDegradedRatio {
			return cursor%2 == 0
		}
	}
	return true
}

// IsQuarantinedOrSubnetCooled reports hard cooldown status without thinning.
func (h *webshareNetworkHealth) IsQuarantinedOrSubnetCooled(asnNumber int, subnet24 string, nowNano int64) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if subnet24 != "" {
		if sh, ok := h.subnets[subnet24]; ok && sh.cooldownUntil > nowNano {
			return true
		}
	}
	if asnNumber > 0 {
		if ah, ok := h.asns[asnNumber]; ok && ah.quarantined && nowNano < ah.cooldownUntil {
			return true
		}
	}
	return false
}

// Snapshot builds a bounded, non-secret summary over the supplied catalog entries.
func (h *webshareNetworkHealth) Snapshot(entries []*webshareDirectEntry, now time.Time) WebshareNetworkSnapshot {
	nowNano := now.UnixNano()
	type asnAgg struct {
		name    string
		ips     int
		subnets map[string]struct{}
	}
	aggs := make(map[int]*asnAgg)
	allSubnets := make(map[string]struct{})
	for _, e := range entries {
		if e == nil {
			continue
		}
		if e.subnet24 != "" {
			allSubnets[e.subnet24] = struct{}{}
		}
		if e.asnNumber <= 0 {
			continue
		}
		a, ok := aggs[e.asnNumber]
		if !ok {
			a = &asnAgg{name: e.asnName, subnets: make(map[string]struct{})}
			aggs[e.asnNumber] = a
		} else if a.name == "" && e.asnName != "" {
			a.name = e.asnName
		}
		a.ips++
		if e.subnet24 != "" {
			a.subnets[e.subnet24] = struct{}{}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	cooledAll := 0
	for sub := range allSubnets {
		if sh, ok := h.subnets[sub]; ok && sh.cooldownUntil > nowNano {
			cooledAll++
		}
	}

	snap := WebshareNetworkSnapshot{
		TotalASNs:       len(aggs),
		TotalSubnets24:  len(allSubnets),
		CooledSubnets24: cooledAll,
		ASNs:            make([]WebshareASNStatus, 0, len(aggs)),
	}
	for asnNum, agg := range aggs {
		cooledSub := 0
		for sub := range agg.subnets {
			if sh, ok := h.subnets[sub]; ok && sh.cooldownUntil > nowNano {
				cooledSub++
			}
		}
		st := WebshareASNStatus{
			ASNNumber:       asnNum,
			ASNName:         agg.name,
			IPs:             agg.ips,
			Subnets24:       len(agg.subnets),
			CooledSubnets24: cooledSub,
			State:           WebshareASNStateHealthy,
		}
		if ah, ok := h.asns[asnNum]; ok {
			state, ratio, samples, rem := ah.stateString(nowNano)
			st.State = state
			st.FailureRatio = ratio
			st.Samples = samples
			st.OKTotal = ah.okTotal
			st.FailTotal = ah.failTotal
			st.CooldownRemainingSeconds = rem
			st.TripsTotal = ah.tripsTotal
			if st.ASNName == "" {
				st.ASNName = ah.name
			}
		}
		switch st.State {
		case WebshareASNStateQuarantined:
			snap.QuarantinedASNs++
		case WebshareASNStateDegraded:
			snap.DegradedASNs++
		default:
			snap.HealthyASNs++
		}
		snap.ASNs = append(snap.ASNs, st)
	}

	// Sort: quarantined first, then degraded, then by IP count descending, then ASN number.
	stateRank := func(s string) int {
		switch s {
		case WebshareASNStateQuarantined:
			return 0
		case WebshareASNStateDegraded:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(snap.ASNs, func(i, j int) bool {
		ri, rj := stateRank(snap.ASNs[i].State), stateRank(snap.ASNs[j].State)
		if ri != rj {
			return ri < rj
		}
		if snap.ASNs[i].IPs != snap.ASNs[j].IPs {
			return snap.ASNs[i].IPs > snap.ASNs[j].IPs
		}
		return snap.ASNs[i].ASNNumber < snap.ASNs[j].ASNNumber
	})
	return snap
}

// WebshareNetworkSnapshotFor returns the live ASN and /24 subnet health
// snapshot for s (or the process's active webshare_multi / webshare_direct
// supplier when s is nil).
func WebshareNetworkSnapshotFor(s Supplier) WebshareNetworkSnapshot {
	if s != nil {
		if p, ok := s.(WebshareNetworkHealthProvider); ok {
			return p.WebshareNetworkHealth()
		}
		return WebshareNetworkSnapshot{}
	}
	if m := activeWebshareMultiSupplier.Load(); m != nil {
		return m.WebshareNetworkHealth()
	}
	if d := activeWebshareDirectSupplier.Load(); d != nil {
		return d.WebshareNetworkHealth()
	}
	return WebshareNetworkSnapshot{}
}

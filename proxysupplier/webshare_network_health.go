package proxysupplier

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tiered /24 subnet and ASN health tuning. Vars so tests can shrink durations.
var (
	webshareSubnetWindow           = 2 * time.Minute
	webshareSubnetFailureThreshold = 2
	webshareSubnetCooldown         = 5 * time.Minute

	webshareASNWindowSize         = 40
	webshareASNDegradedMinSamples = 10
	webshareASNMinSamples         = 20
	webshareASNDegradedRatio      = 0.25
	webshareASNHeavyRatio         = 0.40
	webshareASNSevereRatio        = 0.55
	webshareASNQuarantineRatio    = 0.65
	webshareASNBaseCooldown       = 2 * time.Minute
	webshareASNMaxCooldown        = 10 * time.Minute
	webshareASNProbeInterval      = 15 * time.Second
	webshareASNCloseAfterProbes   = 1
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
	// PacedTotal counts selections that skipped this ASN because its pacing
	// bucket had no token (see WebsharePacing).
	PacedTotal uint64 `json:"paced_total"`
	// PacingRatePerSecond is the ASN's current pacing refill rate in requests
	// per second; 0 when pacing is disabled.
	PacingRatePerSecond float64 `json:"pacing_rate_per_second"`
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
	// Pacing is the per-ASN pacing configuration and counters (see WebsharePacing).
	Pacing WebsharePacingStatus `json:"pacing"`
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

// idle reports whether the subnet holds no state worth keeping at nowNano: no
// failure inside the window and no running cooldown.
func (sh *subnetHealth) idle(nowNano int64) bool {
	if sh.cooldownUntil > nowNano {
		return false
	}
	cutoff := nowNano - webshareSubnetWindow.Nanoseconds()
	for _, ts := range sh.recentFails {
		if ts >= cutoff {
			return false
		}
	}
	return true
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
	// admitted counts thinning decisions for callers that pass no per-entry
	// counter to Allows.
	admitted uint64
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
	if samples >= webshareASNDegradedMinSamples && ratio >= webshareASNDegradedRatio {
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
	// lastPruneNano is when the subnet map was last pruned because it was full.
	lastPruneNano int64

	// pacer holds the opt-in per-ASN token buckets; shared like the health itself.
	pacer *webshareASNPacer
}

func newWebshareNetworkHealth() *webshareNetworkHealth {
	return &webshareNetworkHealth{
		subnets: make(map[string]*subnetHealth),
		asns:    make(map[int]*asnHealth),
		pacer:   newWebshareASNPacer(),
	}
}

// webshareSubnetPruneEvery bounds how often a full subnet map is scanned for
// idle entries from Record (the scan is O(webshareMaxTrackedSubnets)).
const webshareSubnetPruneEvery = time.Second

// PruneSubnets drops every tracked /24 that is idle at now: no failure inside
// webshareSubnetWindow and no running cooldown. Such an entry carries no
// information (a new failure would start it from scratch anyway), and keeping
// it would let the map reach webshareMaxTrackedSubnets and stop tracking new
// subnets for good. Called on every catalog refresh and from Record when the
// map is full.
func (h *webshareNetworkHealth) PruneSubnets(now time.Time) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pruneSubnetsLocked(now.UnixNano())
}

func (h *webshareNetworkHealth) pruneSubnetsLocked(nowNano int64) int {
	h.lastPruneNano = nowNano
	pruned := 0
	for sub, sh := range h.subnets {
		if sh.idle(nowNano) {
			delete(h.subnets, sub)
			pruned++
		}
	}
	return pruned
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
		if ok {
			return
		}
		if len(h.subnets) >= webshareMaxTrackedSubnets && nowNano-h.lastPruneNano >= webshareSubnetPruneEvery.Nanoseconds() {
			h.pruneSubnetsLocked(nowNano)
		}
		if len(h.subnets) >= webshareMaxTrackedSubnets {
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
		if sh.idle(nowNano) {
			delete(h.subnets, subnet24)
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
// on normal pass 1. Degraded ASNs are thinned so they receive 50% (failure
// ratio 25-40%) or 25% (40-50%) of their normal traffic; quarantined ASNs only
// allow a single probe per webshareASNProbeInterval after cooldown expires.
//
// Thinning counts admissions per entry: admit is the candidate entry's own
// counter, and the entry is admitted on every 2nd (or 4th) time it is
// considered while its ASN is degraded. Every IP of the ASN therefore keeps
// rotating at the thinned rate. (It used to test the parity of the selection
// cursor, which for an even-sized list matches the entry's position, so the
// same half of the ASN's IPs was skipped every time.) A nil admit uses one
// counter for the whole ASN.
func (h *webshareNetworkHealth) Allows(asnNumber int, subnet24 string, nowNano int64, admit *atomic.Uint64) bool {
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
	if samples >= webshareASNDegradedMinSamples {
		every := uint64(0)
		if ratio >= webshareASNSevereRatio {
			every = 8
		} else if ratio >= webshareASNHeavyRatio {
			every = 4
		} else if ratio >= webshareASNDegradedRatio {
			every = 2
		}
		if every > 0 {
			var n uint64
			if admit != nil {
				n = admit.Add(1)
			} else {
				ah.admitted++
				n = ah.admitted
			}
			return n%every == 0
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

	pacing, pacedByASN, rates := h.pacer.status()

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
		Pacing:          pacing,
	}
	for asnNum, agg := range aggs {
		cooledSub := 0
		for sub := range agg.subnets {
			if sh, ok := h.subnets[sub]; ok && sh.cooldownUntil > nowNano {
				cooledSub++
			}
		}
		st := WebshareASNStatus{
			ASNNumber:           asnNum,
			ASNName:             agg.name,
			IPs:                 agg.ips,
			Subnets24:           len(agg.subnets),
			CooledSubnets24:     cooledSub,
			State:               WebshareASNStateHealthy,
			PacedTotal:          pacedByASN[asnNum],
			PacingRatePerSecond: rates[asnNum],
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

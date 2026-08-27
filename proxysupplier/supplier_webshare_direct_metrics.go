package proxysupplier

import (
	"sync/atomic"
	"time"
)

// WebshareDirectPoolState is a point-in-time view of the active
// webshare_direct supplier's proxy pool. It is consumed by promx when the
// process's existing /metrics endpoint is scraped.
type WebshareDirectPoolState struct {
	Total      int
	InCooldown int
	Eligible   int
}

// activeWebshareDirectSupplier is the process's configured webshare_direct
// supplier. A fleet service constructs one supplier for its outbound proxy
// path; keeping the pointer here lets promx register its collector before or
// after that construction without losing the initial pool state.
var activeWebshareDirectSupplier atomic.Pointer[webshareDirectSupplier]

// WebshareDirectPoolSnapshot returns a consistent live snapshot of the
// process's active webshare_direct pool. A process with no configured
// webshare_direct supplier reports an all-zero state.
func WebshareDirectPoolSnapshot() WebshareDirectPoolState {
	s := activeWebshareDirectSupplier.Load()
	if s == nil {
		return WebshareDirectPoolState{}
	}
	return s.poolSnapshot(time.Now().UnixNano())
}

// poolSnapshot scans the pool once under its existing read lock. The caller
// supplies the time so cooldown-expiry behavior is deterministic in tests.
// No Prometheus update happens while the lock is held.
func (s *webshareDirectSupplier) poolSnapshot(nowUnixNano int64) WebshareDirectPoolState {
	s.mu.RLock()
	total := len(s.list)
	inCooldown := 0
	for _, e := range s.list {
		if !e.eligible(nowUnixNano) {
			inCooldown++
		}
	}
	s.mu.RUnlock()

	return WebshareDirectPoolState{
		Total:      total,
		InCooldown: inCooldown,
		Eligible:   total - inCooldown,
	}
}

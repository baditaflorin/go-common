package proxysupplier

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// webshareHealthStateVersion is the format version of WebshareHealthState.
const webshareHealthStateVersion = 1

// WebshareHealthState is the durable part of the shared Webshare ASN and /24
// health: which ASNs are quarantined and until when, how often they tripped
// (the cooldown escalates with it), their recent outcome window, and the /24
// subnets still cooling down. A process that restores it after a restart does
// not have to rediscover a challenge wave by sending traffic into every ASN
// again. It holds no proxy address or credential; subnets are /24 prefixes.
type WebshareHealthState struct {
	Version int                         `json:"version"`
	SavedAt time.Time                   `json:"saved_at"`
	ASNs    []WebshareASNHealthState    `json:"asns,omitempty"`
	Subnets []WebshareSubnetHealthState `json:"subnets,omitempty"`
}

// WebshareASNHealthState is one ASN in WebshareHealthState.
type WebshareASNHealthState struct {
	ASN  int    `json:"asn"`
	Name string `json:"name,omitempty"`
	// Window is the recent outcome window, oldest first: '1' ok, '0' failure.
	Window           string    `json:"window,omitempty"`
	Quarantined      bool      `json:"quarantined,omitempty"`
	CooldownUntil    time.Time `json:"cooldown_until,omitzero"`
	ConsecutiveTrips int       `json:"consecutive_trips,omitempty"`
	TripsTotal       uint64    `json:"trips_total,omitempty"`
	OKTotal          uint64    `json:"ok_total,omitempty"`
	FailTotal        uint64    `json:"fail_total,omitempty"`
}

// WebshareSubnetHealthState is one /24 subnet with a running cooldown.
type WebshareSubnetHealthState struct {
	Subnet24      string    `json:"subnet24"`
	CooldownUntil time.Time `json:"cooldown_until"`
}

// WebshareHealthPersister is implemented by the webshare_direct and
// webshare_multi suppliers, in the same optional-capability style as
// WebshareCapacityReporter. ExportHealthState reads the shared health tracker
// without changing it. RestoreHealthState adds the ASNs and subnets of st that
// the tracker does not know yet (it never overwrites live state) and returns
// how many it restored; call it at startup, before traffic.
type WebshareHealthPersister interface {
	ExportHealthState(now time.Time) WebshareHealthState
	RestoreHealthState(st WebshareHealthState, now time.Time) (asns, subnets int)
}

func (h *webshareNetworkHealth) exportState(now time.Time) WebshareHealthState {
	st := WebshareHealthState{Version: webshareHealthStateVersion, SavedAt: now.UTC()}
	if h == nil {
		return st
	}
	nowNano := now.UnixNano()
	h.mu.Lock()
	defer h.mu.Unlock()
	for asn, ah := range h.asns {
		a := WebshareASNHealthState{
			ASN: asn, Name: ah.name, Window: ah.windowString(),
			Quarantined: ah.quarantined, ConsecutiveTrips: ah.consecutiveTrips,
			TripsTotal: ah.tripsTotal, OKTotal: ah.okTotal, FailTotal: ah.failTotal,
		}
		if ah.cooldownUntil > 0 {
			a.CooldownUntil = time.Unix(0, ah.cooldownUntil).UTC()
		}
		st.ASNs = append(st.ASNs, a)
	}
	for sub, sh := range h.subnets {
		if sh.cooldownUntil > nowNano {
			st.Subnets = append(st.Subnets, WebshareSubnetHealthState{Subnet24: sub, CooldownUntil: time.Unix(0, sh.cooldownUntil).UTC()})
		}
	}
	return st
}

// windowString renders the outcome ring buffer oldest first.
func (a *asnHealth) windowString() string {
	if a.count == 0 || len(a.outcomes) == 0 {
		return ""
	}
	var b strings.Builder
	start := 0
	if a.count == len(a.outcomes) {
		start = a.pos
	}
	for i := 0; i < a.count; i++ {
		if a.outcomes[(start+i)%len(a.outcomes)] {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

func (h *webshareNetworkHealth) restoreState(st WebshareHealthState, now time.Time) (asns, subnets int) {
	if h == nil || st.Version != webshareHealthStateVersion {
		return 0, 0
	}
	nowNano := now.UnixNano()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range st.ASNs {
		if a.ASN <= 0 {
			continue
		}
		if _, known := h.asns[a.ASN]; known || len(h.asns) >= webshareMaxTrackedASNs {
			continue
		}
		ah := &asnHealth{
			name: strings.TrimSpace(a.Name), quarantined: a.Quarantined, consecutiveTrips: a.ConsecutiveTrips,
			tripsTotal: a.TripsTotal, okTotal: a.OKTotal, failTotal: a.FailTotal,
		}
		if a.Quarantined && !a.CooldownUntil.IsZero() {
			ah.cooldownUntil = a.CooldownUntil.UnixNano()
		}
		for _, c := range a.Window {
			ah.recordOutcome(c == '1')
		}
		h.asns[a.ASN] = ah
		asns++
	}
	for _, s := range st.Subnets {
		until := s.CooldownUntil.UnixNano()
		if s.Subnet24 == "" || until <= nowNano {
			continue
		}
		if _, known := h.subnets[s.Subnet24]; known || len(h.subnets) >= webshareMaxTrackedSubnets {
			continue
		}
		h.subnets[s.Subnet24] = &subnetHealth{recentFails: make(map[string]int64), cooldownUntil: until}
		subnets++
	}
	return asns, subnets
}

// ExportHealthState implements WebshareHealthPersister.
func (s *webshareMultiSupplier) ExportHealthState(now time.Time) WebshareHealthState {
	if s == nil {
		return WebshareHealthState{Version: webshareHealthStateVersion, SavedAt: now.UTC()}
	}
	return s.networkHealth.exportState(now)
}

// RestoreHealthState implements WebshareHealthPersister.
func (s *webshareMultiSupplier) RestoreHealthState(st WebshareHealthState, now time.Time) (int, int) {
	if s == nil {
		return 0, 0
	}
	return s.networkHealth.restoreState(st, now)
}

// ExportHealthState implements WebshareHealthPersister.
func (s *webshareDirectSupplier) ExportHealthState(now time.Time) WebshareHealthState {
	if s == nil {
		return WebshareHealthState{Version: webshareHealthStateVersion, SavedAt: now.UTC()}
	}
	return s.networkHealth.exportState(now)
}

// RestoreHealthState implements WebshareHealthPersister.
func (s *webshareDirectSupplier) RestoreHealthState(st WebshareHealthState, now time.Time) (int, int) {
	if s == nil {
		return 0, 0
	}
	return s.networkHealth.restoreState(st, now)
}

var (
	_ WebshareHealthPersister = (*webshareMultiSupplier)(nil)
	_ WebshareHealthPersister = (*webshareDirectSupplier)(nil)
)

// WebshareHealthFileOptions tunes WebshareHealthFile. Zero values take the
// defaults.
type WebshareHealthFileOptions struct {
	// Interval between saves (default 10s).
	Interval time.Duration
	// MaxAge: a saved state older than this is ignored at open (default 2h,
	// four times the longest ASN cooldown).
	MaxAge time.Duration
	// Logf receives save errors (default: dropped).
	Logf func(format string, args ...any)
}

// WebshareHealthRestore says what OpenWebshareHealthFile restored.
type WebshareHealthRestore struct {
	ASNs, Subnets24, QuarantinedASNs int
	// SavedAt is when the restored state was written; zero when nothing was read.
	SavedAt time.Time
	// Skipped says why a file that exists was not restored ("" when it was, or
	// when there was no file).
	Skipped string
}

// WebshareHealthFile keeps a supplier's ASN and /24 health in a JSON file across
// restarts: OpenWebshareHealthFile restores it, then saves it every Interval
// (write to a temporary file, fsync, rename, mode 0600); Close stops the loop
// and saves once more. A missing, unreadable, foreign-version or too old file
// is not an error: the supplier starts with fresh health, as without the file.
type WebshareHealthFile struct {
	p    WebshareHealthPersister
	path string
	opts WebshareHealthFileOptions
	stop chan struct{}
	done chan struct{}
	once sync.Once
	mu   sync.Mutex // serializes saves
}

// OpenWebshareHealthFile restores p from path and starts the periodic save.
// It returns an error only when the file's directory cannot be written, so
// that a mount mistake shows at startup instead of as a silent loss.
func OpenWebshareHealthFile(p WebshareHealthPersister, path string, opts WebshareHealthFileOptions) (*WebshareHealthFile, WebshareHealthRestore, error) {
	var res WebshareHealthRestore
	if p == nil || strings.TrimSpace(path) == "" {
		return nil, res, errors.New("proxysupplier: webshare health file needs a supplier and a path")
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = 2 * time.Hour
	}
	now := time.Now()
	if raw, err := os.ReadFile(path); err == nil {
		var st WebshareHealthState
		switch {
		case json.Unmarshal(raw, &st) != nil:
			res.Skipped = "unreadable"
		case st.Version != webshareHealthStateVersion:
			res.Skipped = fmt.Sprintf("version %d", st.Version)
		case now.Sub(st.SavedAt) > opts.MaxAge:
			res.Skipped = fmt.Sprintf("older than %s", opts.MaxAge)
		default:
			res.ASNs, res.Subnets24 = p.RestoreHealthState(st, now)
			res.SavedAt = st.SavedAt
			for _, a := range st.ASNs {
				if a.Quarantined {
					res.QuarantinedASNs++
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		res.Skipped = "unreadable"
	}
	f := &WebshareHealthFile{p: p, path: path, opts: opts, stop: make(chan struct{}), done: make(chan struct{})}
	if err := f.Save(); err != nil {
		return nil, res, err
	}
	go f.loop()
	return f, res, nil
}

func (f *WebshareHealthFile) loop() {
	defer close(f.done)
	t := time.NewTicker(f.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			if err := f.Save(); err != nil && f.opts.Logf != nil {
				f.opts.Logf("webshare health file: save: %v", err)
			}
		}
	}
}

// Save writes the current state now.
func (f *WebshareHealthFile) Save() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(f.p.ExportHealthState(time.Now()))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), "."+filepath.Base(f.path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, f.path)
}

// Close stops the periodic save and saves once more. Safe to call twice.
func (f *WebshareHealthFile) Close() error {
	if f == nil {
		return nil
	}
	var err error
	f.once.Do(func() {
		close(f.stop)
		<-f.done
		err = f.Save()
	})
	return err
}

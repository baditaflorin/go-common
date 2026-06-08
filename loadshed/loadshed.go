package loadshed

import (
	"sync"
	"sync/atomic"
)

// Gate is a non-blocking concurrency limiter. Construct with New; safe
// for concurrent TryAcquire calls. A nil-sem Gate (limit <= 0) is
// unbounded: TryAcquire always admits. The zero value is not usable —
// always go through New.
type Gate struct {
	name string
	sem  chan struct{} // nil => unbounded

	inFlight atomic.Int64
	admitted atomic.Int64 // total successful TryAcquire admissions
	shed     atomic.Int64 // total TryAcquire refusals (gate full)
}

// New constructs a Gate that admits at most `limit` concurrent callers.
// limit <= 0 leaves the gate unbounded (TryAcquire always succeeds) —
// the test default and a clean opt-out. name is the gate slug
// ("render", "screenshot") used as a metric label; keep it short and
// stable. An empty name folds to "_unnamed".
func New(name string, limit int) *Gate {
	if name == "" {
		name = "_unnamed"
	}
	g := &Gate{name: name}
	if limit > 0 {
		g.sem = make(chan struct{}, limit)
	}
	return g
}

// Name returns the gate slug.
func (g *Gate) Name() string { return g.name }

// Limit returns the configured concurrency cap, or 0 if the gate is
// unbounded.
func (g *Gate) Limit() int { return cap(g.sem) }

// InFlight returns the current number of admitted-but-not-yet-released
// callers.
func (g *Gate) InFlight() int64 { return g.inFlight.Load() }

// AdmittedTotal returns the cumulative number of admitted callers.
func (g *Gate) AdmittedTotal() int64 { return g.admitted.Load() }

// ShedTotal returns the cumulative number of shed (refused) callers —
// the canonical load-shed counter.
func (g *Gate) ShedTotal() int64 { return g.shed.Load() }

var noop = func() {}

// TryAcquire attempts to take a slot WITHOUT blocking. On success it
// returns a release func (call exactly once — typically `defer
// release()`) and true. On failure (the gate is full) it records a
// shed and returns a no-op release and false; the caller should fail
// fast (e.g. loadshed.WriteShed) rather than proceed to the upstream.
//
// The returned release is idempotent and always safe to call, including
// after a shed — so `release, ok := g.TryAcquire(); defer release()`
// before the `if !ok` check will not double-release.
func (g *Gate) TryAcquire() (release func(), ok bool) {
	if g.sem == nil {
		// Unbounded: admit unconditionally but still track in-flight so
		// metrics/observers see real concurrency.
		g.admitted.Add(1)
		g.inFlight.Add(1)
		g.emit(PhaseAdmitted)
		return g.releaser(false), true
	}
	select {
	case g.sem <- struct{}{}:
		g.admitted.Add(1)
		g.inFlight.Add(1)
		g.emit(PhaseAdmitted)
		return g.releaser(true), true
	default:
		g.shed.Add(1)
		g.emit(PhaseShed)
		return noop, false
	}
}

// releaser builds an idempotent release closure. drain reports whether
// a slot must be returned to the semaphore on release (false for the
// unbounded path).
func (g *Gate) releaser(drain bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if drain {
				<-g.sem
			}
			g.inFlight.Add(-1)
			g.emit(PhaseReleased)
		})
	}
}

// Phase buckets the observer event types.
type Phase string

const (
	PhaseAdmitted Phase = "admitted" // TryAcquire obtained a slot
	PhaseReleased Phase = "released" // an admitted caller released its slot
	PhaseShed     Phase = "shed"     // TryAcquire refused because the gate was full
)

// Observer receives one event per TryAcquire / release transition.
// Implementations MUST NOT block.
type Observer interface {
	ObserveLoadshed(Event)
}

// Event is the payload handed to an Observer on each transition.
type Event struct {
	Gate     string
	Phase    Phase
	Limit    int
	InFlight int64
}

var defaultObserver atomic.Pointer[Observer]

// SetDefaultObserver installs a process-wide observer. Pass nil to
// disable. Wired by promx.AutoWire.
func SetDefaultObserver(o Observer) {
	if o == nil {
		defaultObserver.Store(nil)
		return
	}
	defaultObserver.Store(&o)
}

// DefaultObserver returns the current process-wide observer or nil.
func DefaultObserver() Observer {
	p := defaultObserver.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (g *Gate) emit(phase Phase) {
	obs := DefaultObserver()
	if obs == nil {
		return
	}
	obs.ObserveLoadshed(Event{
		Gate:     g.name,
		Phase:    phase,
		Limit:    cap(g.sem),
		InFlight: g.inFlight.Load(),
	})
}

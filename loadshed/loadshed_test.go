package loadshed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGate_AdmitsUpToLimitThenSheds(t *testing.T) {
	g := New("render", 2)

	r1, ok1 := g.TryAcquire()
	r2, ok2 := g.TryAcquire()
	if !ok1 || !ok2 {
		t.Fatalf("first two acquires must succeed, got %v %v", ok1, ok2)
	}
	if g.InFlight() != 2 {
		t.Fatalf("want 2 in-flight, got %d", g.InFlight())
	}

	r3, ok3 := g.TryAcquire()
	if ok3 {
		t.Fatal("third acquire must be shed when limit=2 is full")
	}
	if g.ShedTotal() != 1 {
		t.Fatalf("want shed=1, got %d", g.ShedTotal())
	}
	// Releasing once frees a slot for a new acquire.
	r1()
	if g.InFlight() != 1 {
		t.Fatalf("want 1 in-flight after release, got %d", g.InFlight())
	}
	r4, ok4 := g.TryAcquire()
	if !ok4 {
		t.Fatal("acquire after a release must succeed")
	}
	if g.AdmittedTotal() != 3 {
		t.Fatalf("want admitted=3, got %d", g.AdmittedTotal())
	}

	// release everything; double-release of r3 (the shed no-op) is safe.
	r2()
	r3()
	r4()
	if g.InFlight() != 0 {
		t.Fatalf("want 0 in-flight at end, got %d", g.InFlight())
	}
}

func TestGate_ReleaseIsIdempotent(t *testing.T) {
	g := New("x", 1)
	rel, ok := g.TryAcquire()
	if !ok {
		t.Fatal("acquire must succeed")
	}
	rel()
	rel() // second call must be a no-op, not underflow in-flight or panic on <-sem
	if g.InFlight() != 0 {
		t.Fatalf("want 0 in-flight, got %d", g.InFlight())
	}
	// The slot must be reusable exactly once, not twice.
	if _, ok := g.TryAcquire(); !ok {
		t.Fatal("slot must be free again after release")
	}
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("double-release must not have leaked a second slot")
	}
}

func TestGate_UnboundedNeverSheds(t *testing.T) {
	g := New("unbounded", 0)
	if g.Limit() != 0 {
		t.Fatalf("want Limit()=0 for unbounded gate, got %d", g.Limit())
	}
	var releases []func()
	for i := 0; i < 1000; i++ {
		rel, ok := g.TryAcquire()
		if !ok {
			t.Fatalf("unbounded gate must never shed (failed at %d)", i)
		}
		releases = append(releases, rel)
	}
	if g.InFlight() != 1000 {
		t.Fatalf("want 1000 in-flight, got %d", g.InFlight())
	}
	if g.ShedTotal() != 0 {
		t.Fatalf("unbounded gate must record 0 sheds, got %d", g.ShedTotal())
	}
	for _, rel := range releases {
		rel()
	}
	if g.InFlight() != 0 {
		t.Fatalf("want 0 in-flight after releasing all, got %d", g.InFlight())
	}
}

func TestGate_ConcurrentAcquireNeverExceedsLimit(t *testing.T) {
	const limit = 8
	g := New("race", limit)
	var wg sync.WaitGroup
	var maxSeen atomic64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, ok := g.TryAcquire()
			if !ok {
				return
			}
			maxSeen.max(g.InFlight())
			rel()
		}()
	}
	wg.Wait()
	if g.InFlight() != 0 {
		t.Fatalf("want 0 in-flight after all goroutines done, got %d", g.InFlight())
	}
	if maxSeen.load() > limit {
		t.Fatalf("in-flight exceeded limit %d, peaked at %d", limit, maxSeen.load())
	}
	if g.AdmittedTotal()+g.ShedTotal() != 200 {
		t.Fatalf("every acquire must be admitted or shed; admitted=%d shed=%d",
			g.AdmittedTotal(), g.ShedTotal())
	}
}

func TestObserver_ReceivesPhases(t *testing.T) {
	rec := &recordingObserver{}
	SetDefaultObserver(rec)
	defer SetDefaultObserver(nil)

	g := New("obs", 1)
	rel, _ := g.TryAcquire() // admitted
	_, _ = g.TryAcquire()    // shed (limit full)
	rel()                    // released

	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []Phase{PhaseAdmitted, PhaseShed, PhaseReleased}
	if len(rec.events) != len(want) {
		t.Fatalf("want %d events, got %d: %+v", len(want), len(rec.events), rec.events)
	}
	for i, p := range want {
		if rec.events[i].Phase != p {
			t.Fatalf("event %d: want phase %s, got %s", i, p, rec.events[i].Phase)
		}
		if rec.events[i].Gate != "obs" {
			t.Fatalf("event %d: want gate label 'obs', got %q", i, rec.events[i].Gate)
		}
		if rec.events[i].Limit != 1 {
			t.Fatalf("event %d: want Limit=1, got %d", i, rec.events[i].Limit)
		}
	}
}

func TestWriteShed_Wire(t *testing.T) {
	w := httptest.NewRecorder()
	WriteShed(w, 0, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("want Retry-After=1 (default), got %q", got)
	}
	var env struct {
		Status string `json:"status"`
		Error  struct {
			Code      int    `json:"code"`
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("shed body must be a JSON envelope: %v (%s)", err, w.Body.String())
	}
	if env.Status != "error" || env.Error.ErrorCode != ShedErrorCode || env.Error.Code != 503 {
		t.Fatalf("unexpected shed envelope: %+v", env)
	}
}

func TestGuard_GatesWholeHandler(t *testing.T) {
	g := New("guarded", 1)
	// Occupy the only slot so the guarded request is shed.
	occupy, ok := g.TryAcquire()
	if !ok {
		t.Fatal("setup acquire must succeed")
	}
	defer occupy()

	var ran bool
	h := g.Guard(2, "busy")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if ran {
		t.Fatal("guarded handler must NOT run when the gate is full")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 from guard, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("want Retry-After=2 (custom), got %q", got)
	}

	// Free the slot — now the guarded handler runs.
	occupy()
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))
	if !ran || w2.Code != http.StatusOK {
		t.Fatalf("guarded handler must run when a slot is free: ran=%v code=%d", ran, w2.Code)
	}
	if g.InFlight() != 0 {
		t.Fatalf("guard must release its slot on return; in-flight=%d", g.InFlight())
	}
}

// --- test helpers ---

type recordingObserver struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingObserver) ObserveLoadshed(ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

// atomic64 is a tiny max-tracking helper for the race test.
type atomic64 struct {
	mu sync.Mutex
	v  int64
}

func (a *atomic64) max(n int64) {
	a.mu.Lock()
	if n > a.v {
		a.v = n
	}
	a.mu.Unlock()
}

func (a *atomic64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

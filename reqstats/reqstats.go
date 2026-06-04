// Package reqstats emits per-request resource stats on every HTTP response: a
// W3C Server-Timing header (human/devtools) and an X-Request-Stats JSON header
// (machine/cost-engine). It is wired as a default middleware by
// go-common/server, so every service gets the universal payload — total_ms +
// bytes + an approx process-delta block — for free; services enrich with named
// phases, a render block (chromedp), and upstream nesting via the
// request-context Tracker.
//
// See docs/adr/0001-per-request-resource-stats.md. The approx block is a
// best-effort, concurrency-contaminated hint — never the cost basis.
package reqstats

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const approxNote = "process-wide getrusage/alloc deltas — over-attributed under concurrency; NOT for billing"

// Bytes counts request/response/upstream payload sizes.
type Bytes struct {
	In       int64 `json:"in"`
	Out      int64 `json:"out"`
	Upstream int64 `json:"upstream,omitempty"`
}

// Approx is the best-effort, debug-only process-delta block (never billable).
type Approx struct {
	ProcCPUms      int64  `json:"proc_cpu_ms"`
	HeapAllocDelta int64  `json:"heap_alloc_delta,omitempty"`
	Note           string `json:"note"`
}

// Render is the per-tab render-engine block. chromedp services fill it from CDP
// Performance.getMetrics; go-common never imports chromedp — callers pass plain
// numbers. Durations are milliseconds.
type Render struct {
	ScriptMs      float64 `json:"script_ms,omitempty"`
	TaskMs        float64 `json:"task_ms,omitempty"`
	LayoutMs      float64 `json:"layout_ms,omitempty"`
	RecalcStyleMs float64 `json:"recalc_style_ms,omitempty"`
	JSHeapUsed    int64   `json:"js_heap_used,omitempty"`
	JSHeapTotal   int64   `json:"js_heap_total,omitempty"`
	Nodes         int64   `json:"nodes,omitempty"`
	Documents     int64   `json:"documents,omitempty"`
	LayoutCount   int64   `json:"layout_count,omitempty"`
	Frames        int64   `json:"frames,omitempty"`
}

// Stats is the canonical X-Request-Stats envelope.
type Stats struct {
	Svc      string           `json:"svc"`
	Ver      string           `json:"ver"`
	OK       bool             `json:"ok"`
	TotalMs  int64            `json:"total_ms"`
	Bytes    Bytes            `json:"bytes"`
	Approx   *Approx          `json:"approx,omitempty"`
	Phase    map[string]int64 `json:"phase,omitempty"`
	Render   *Render          `json:"render,omitempty"`
	Upstream json.RawMessage  `json:"upstream,omitempty"`
}

// Tracker accumulates per-request stats. Enrichment methods are safe to call
// from the handler goroutine; a Tracker is one-per-request, not shared.
type Tracker struct {
	svc, ver string
	start    time.Time

	approxCPU  bool
	approxHeap bool
	startCPU   time.Duration
	startAlloc uint64

	mu        sync.Mutex
	phases    map[string]int64
	bytesIn   int64
	bytesOut  int64
	upstream  json.RawMessage
	render    *Render
	finalized bool
	snapshot  Stats
}

// Start begins a tracker labeled with the service id + version.
func Start(svc, ver string) *Tracker {
	return &Tracker{svc: svc, ver: ver, start: time.Now(), phases: map[string]int64{}}
}

// EnableApprox turns on the cheap process-CPU delta (getrusage). Default-on in
// the server middleware.
func (t *Tracker) EnableApprox() *Tracker {
	t.approxCPU = true
	t.startCPU = procCPU()
	return t
}

// EnableHeapDelta also records the per-request heap-alloc delta. This calls
// runtime.ReadMemStats (a small stop-the-world) per request — opt-in for
// services that want a RAM hint and can afford it; high-QPS services should
// leave it off.
func (t *Tracker) EnableHeapDelta() *Tracker {
	t.approxHeap = true
	t.startAlloc = heapAlloc()
	return t
}

// Phase times a named span; call the returned func to record it. Repeated names
// accumulate.
func (t *Tracker) Phase(name string) func() {
	s := time.Now()
	return func() { t.Mark(name, time.Since(s)) }
}

// Mark records a named duration directly.
func (t *Tracker) Mark(name string, d time.Duration) {
	t.mu.Lock()
	t.phases[name] += d.Milliseconds()
	t.mu.Unlock()
}

func (t *Tracker) AddBytesIn(n int64)  { t.mu.Lock(); t.bytesIn += n; t.mu.Unlock() }
func (t *Tracker) AddBytesOut(n int64) { t.mu.Lock(); t.bytesOut += n; t.mu.Unlock() }
func (t *Tracker) SetBytesOut(n int64) { t.mu.Lock(); t.bytesOut = n; t.mu.Unlock() }

// SetRender attaches the render-engine block (chromedp services).
func (t *Tracker) SetRender(r Render) {
	rr := r
	t.mu.Lock()
	t.render = &rr
	t.mu.Unlock()
}

// SetUpstream nests a callee's X-Request-Stats header value under "upstream".
// Invalid/empty JSON is ignored.
func (t *Tracker) SetUpstream(headerVal string) {
	if headerVal == "" || !json.Valid([]byte(headerVal)) {
		return
	}
	t.mu.Lock()
	t.upstream = json.RawMessage(headerVal)
	t.mu.Unlock()
}

// finalize computes total + approx exactly once and snapshots Stats.
func (t *Tracker) finalize(ok bool) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalized {
		return t.snapshot
	}
	t.finalized = true
	s := Stats{
		Svc:      t.svc,
		Ver:      t.ver,
		OK:       ok,
		TotalMs:  time.Since(t.start).Milliseconds(),
		Bytes:    Bytes{In: t.bytesIn, Out: t.bytesOut},
		Render:   t.render,
		Upstream: t.upstream,
	}
	if len(t.phases) > 0 {
		s.Phase = t.phases
	}
	if t.approxCPU || t.approxHeap {
		a := &Approx{Note: approxNote}
		if t.approxCPU {
			a.ProcCPUms = (procCPU() - t.startCPU).Milliseconds()
		}
		if t.approxHeap {
			a.HeapAllocDelta = int64(heapAlloc() - t.startAlloc)
		}
		s.Approx = a
	}
	t.snapshot = s
	return s
}

// Stats returns the finalized snapshot (finalizing with ok=true if needed).
func (t *Tracker) Stats() Stats { return t.finalize(true) }

// writeHeaders injects Server-Timing + X-Request-Stats into h, finalizing with
// the given ok. Called once, before the body is flushed.
func (t *Tracker) writeHeaders(h http.Header, ok bool) {
	s := t.finalize(ok)
	if b, err := json.Marshal(s); err == nil {
		h.Set("X-Request-Stats", string(b))
	}
	h.Set("Server-Timing", serverTiming(s))
}

// serverTiming renders the W3C Server-Timing header from the stats.
func serverTiming(s Stats) string {
	var b strings.Builder
	b.WriteString("total;dur=")
	b.WriteString(strconv.FormatInt(s.TotalMs, 10))
	keys := make([]string, 0, len(s.Phase))
	for k := range s.Phase {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		b.WriteString(", ")
		b.WriteString(metricToken(name))
		b.WriteString(";dur=")
		b.WriteString(strconv.FormatInt(s.Phase[name], 10))
	}
	if s.Render != nil && s.Render.TaskMs > 0 {
		b.WriteString(`, render_cpu;dur=`)
		b.WriteString(strconv.FormatInt(int64(s.Render.TaskMs), 10))
	}
	if s.Approx != nil {
		b.WriteString(", proc_cpu;dur=")
		b.WriteString(strconv.FormatInt(s.Approx.ProcCPUms, 10))
	}
	return b.String()
}

// metricToken sanitizes a phase name into a Server-Timing token (alnum, -, _).
func metricToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "phase"
	}
	return b.String()
}

// --- request-context plumbing ---

type ctxKey struct{}

// NewContext stores the tracker on the context (the middleware does this).
func NewContext(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// From returns the request's Tracker, or a detached no-op tracker if absent, so
// callers can enrich unconditionally without nil checks.
func From(ctx context.Context) *Tracker {
	if t, ok := ctx.Value(ctxKey{}).(*Tracker); ok && t != nil {
		return t
	}
	return Start("", "")
}

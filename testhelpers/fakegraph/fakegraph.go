// Package fakegraph captures graph.Event values for use in tests.
// It implements the graph Observer interface, recording every event
// emitted by graph.RoundTripper and graph.Middleware so tests can
// assert on call counts, targets, status codes, and latencies without
// a real collector.
//
// Usage:
//
//	fg := fakegraph.New()
//	graph.SetDefaultRecorder(fg)  // or pass directly to graph.Config
//
//	// after running the code under test:
//	events := fg.Events()
//	if len(events) != 1 { t.Fatal("expected one outbound event") }
//	if events[0].Target != "go_extractor" { t.Fatal("wrong target") }
//
//	fg.Reset() // clear between test cases
package fakegraph

import (
	"sync"

	"github.com/baditaflorin/go-common/graph"
)

// Recorder captures graph.Event values emitted during a test.
// All methods are safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	events []graph.Event
}

// New returns an empty Recorder.
func New() *Recorder { return &Recorder{} }

// Record appends the event to the internal log. Implements graph.Recorder.
func (r *Recorder) Record(e graph.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a snapshot copy of all recorded events in order.
func (r *Recorder) Events() []graph.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]graph.Event, len(r.events))
	copy(out, r.events)
	return out
}

// Len returns the number of recorded events.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// Reset clears all recorded events.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = r.events[:0]
}

// Filter returns all events matching predicate fn.
func (r *Recorder) Filter(fn func(graph.Event) bool) []graph.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []graph.Event
	for _, e := range r.events {
		if fn(e) {
			out = append(out, e)
		}
	}
	return out
}

// OutboundTo returns all "out" direction events whose Target equals target.
func (r *Recorder) OutboundTo(target string) []graph.Event {
	return r.Filter(func(e graph.Event) bool {
		return e.Direction == "out" && e.Target == target
	})
}

// InboundFrom returns all "in" direction events whose Caller equals caller.
func (r *Recorder) InboundFrom(caller string) []graph.Event {
	return r.Filter(func(e graph.Event) bool {
		return e.Direction == "in" && e.Caller == caller
	})
}

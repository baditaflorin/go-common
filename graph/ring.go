package graph

import "sync"

// ring is a fixed-capacity FIFO with drop-oldest semantics. We prefer
// drop-oldest over drop-newest so that during a collector outage the
// most recent observations survive — they're more interesting than
// stale ones we couldn't ship anyway.
type ring struct {
	mu   sync.Mutex
	buf  []Event
	head int // next write index
	tail int // next read index
	size int // current count
	cap  int
}

func newRing(cap int) *ring {
	if cap < 1 {
		cap = 1
	}
	return &ring{buf: make([]Event, cap), cap: cap}
}

// push appends e. Returns (added bool, droppedOld bool).
// droppedOld is true when the ring was full and the oldest event
// was evicted to make room.
func (r *ring) push(e Event) (bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dropped := false
	if r.size == r.cap {
		// Evict oldest by advancing tail.
		r.tail = (r.tail + 1) % r.cap
		r.size--
		dropped = true
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % r.cap
	r.size++
	return true, dropped
}

// drain removes up to max events and returns them in FIFO order.
// Returns nil if the ring is empty.
func (r *ring) drain(max int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	if max <= 0 || max > r.size {
		max = r.size
	}
	out := make([]Event, max)
	for i := 0; i < max; i++ {
		out[i] = r.buf[r.tail]
		r.tail = (r.tail + 1) % r.cap
	}
	r.size -= max
	return out
}

// len returns the current count without taking the lock long.
func (r *ring) len() int {
	r.mu.Lock()
	n := r.size
	r.mu.Unlock()
	return n
}

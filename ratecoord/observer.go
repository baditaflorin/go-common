package ratecoord

import "time"

// Observer receives one event per Client.Wait call. Implementations
// MUST NOT block — callbacks run inline on the request hot path of any
// service fanning out HTTP traffic. The canonical implementation lives
// in go-common/promx.
//
// ratecoord deliberately defines the contract here rather than
// importing a metrics library: ratecoord keeps zero metric-stack deps.
type Observer interface {
	ObserveRate(Event)
}

// Event is the per-call payload handed to an Observer.
//
// Allowed is true when a token was acquired (whether remotely or via
// the in-process fallback). FellBack records whether the local bucket
// served the answer because the coordinator was unreachable.
type Event struct {
	Host     string
	Weight   int
	Waited   time.Duration
	Allowed  bool
	FellBack bool
	Reason   string
}

// SetObserver attaches an Observer to the client. Idempotent.
func (c *Client) SetObserver(o Observer) *Client {
	c.observer = o
	return c
}

package fleetfetch

import "time"

// nowFn is overridable in tests for FetchedAt determinism.
var nowFn = func() time.Time { return time.Now().UTC() }

var errLoopbackEmptyURL = newLoopbackErr("fleetfetch: empty target url")

func newLoopbackErr(msg string) error { return loopbackErr(msg) }

type loopbackErr string

func (e loopbackErr) Error() string { return string(e) }

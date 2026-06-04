package safehttp

import (
	"time"
)

// WithPersistInterval sets how often the breaker state is flushed
// to disk while the process is running. Default 60s. Values below
// 100ms are clamped to 100ms — a tighter loop is almost certainly
// a test artefact.
func WithPersistInterval(d time.Duration) BreakerStateOption {
	return func(c *breakerStateConfig) { c.persistInterval = d }
}

// WithSaveOnShutdown controls whether ShutdownBreakerState flushes
// once more on the way out. Default true; pass false if the caller
// owns shutdown ordering and wants to skip the final write.
func WithSaveOnShutdown(save bool) BreakerStateOption {
	return func(c *breakerStateConfig) { c.saveOnShutdown = save }
}

// WithPersistentBreakerState wires the local backoff-coordinator
// client state (the last-known-bad host list) to a disk-backed
// store at the given path. Only meaningful when paired with
// WithBackoffCoordinator — without it, there is no state to persist.
//
// At construction the file (if present) is read and the in-memory
// state is warmed with it; on a periodic ticker (default 60s) and
// on ShutdownBreakerState, the state is written atomically (write
// to "<path>.tmp" then rename) so a crash mid-write cannot corrupt
// the file.
//
// All failure modes are warnings, never fatal: persistence is
// best-effort observability, not load-bearing infrastructure.
//
// Typical use:
//
//	cli := safehttp.NewClient(
//	    safehttp.WithBackoffCoordinator(os.Getenv("BACKOFF_COORDINATOR_URL")),
//	    safehttp.WithPersistentBreakerState("/var/lib/myservice/breaker.json"),
//	)
//	defer safehttp.ShutdownBreakerState(cli)
func WithPersistentBreakerState(path string, opts ...BreakerStateOption) Option {
	cfg := breakerStateConfig{
		path:            path,
		persistInterval: 60 * time.Second,
		saveOnShutdown:  true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.persistInterval > 0 && cfg.persistInterval < 100*time.Millisecond {
		cfg.persistInterval = 100 * time.Millisecond
	}
	return func(o *options) { o.breakerState = &cfg }
}

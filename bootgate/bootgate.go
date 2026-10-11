package bootgate

import (
	"context"
	"hash/fnv"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/env"
)

// Environment variables read by FromEnv.
const (
	EnvBaseDelay = "BOOT_BASE_DELAY"
	EnvJitterMax = "BOOT_JITTER_MAX"
	EnvMaxLoad   = "BOOT_MAX_LOAD"
	EnvMaxWait   = "BOOT_MAX_WAIT"
	EnvPollEvery = "BOOT_POLL_EVERY"
)

// Defaults applied by Wait when the load gate is enabled.
const (
	// DefaultMaxWait caps the load gate when MaxLoad > 0 and MaxWait is 0.
	DefaultMaxWait = 5 * time.Minute
	// DefaultPollEvery is the load poll interval when PollEvery is 0.
	DefaultPollEvery = 5 * time.Second
)

// Config controls Wait. The zero value disables everything.
type Config struct {
	// Name identifies the service in logs and is the fallback Seed.
	Name string
	// BaseDelay is a fixed delay applied before the jitter.
	BaseDelay time.Duration
	// JitterMax is the exclusive upper bound of the deterministic jitter.
	// 0 disables jitter.
	JitterMax time.Duration
	// MaxLoad is the 1-minute load average the host must be below before
	// Wait returns. 0 disables the load gate.
	MaxLoad float64
	// MaxWait caps how long the load gate may block (measured from when the
	// gate starts, after BaseDelay and jitter). 0 means DefaultMaxWait.
	MaxWait time.Duration
	// PollEvery is the load poll interval. 0 means DefaultPollEvery.
	PollEvery time.Duration
	// Seed is hashed to derive the jitter. Empty means the hostname, then
	// Name.
	Seed string
}

// Result describes what Wait did, so callers can log it.
type Result struct {
	// Waited is the total time spent in Wait.
	Waited time.Duration
	// Jitter is the deterministic jitter that was applied.
	Jitter time.Duration
	// LoadAtStart and LoadAtEnd are the first and last load samples; zero
	// when LoadKnown is false.
	LoadAtStart, LoadAtEnd float64
	// TimedOut is true if MaxWait elapsed with the load still too high.
	TimedOut bool
	// LoadKnown is true if a load average could be read.
	LoadKnown bool
}

// Injectable for tests.
var (
	now      = time.Now
	sleep    = realSleep
	readLoad = readHostLoad
	hostname = os.Hostname
)

// FromEnv builds a Config for the named service from BOOT_* environment
// variables. Unset, invalid, or negative values leave the knob disabled.
func FromEnv(name string) Config {
	c := Config{
		Name:      name,
		BaseDelay: env.Duration(EnvBaseDelay, 0),
		JitterMax: env.Duration(EnvJitterMax, 0),
		MaxLoad:   env.Float64(EnvMaxLoad, 0),
		MaxWait:   env.Duration(EnvMaxWait, 0),
		PollEvery: env.Duration(EnvPollEvery, 0),
	}
	return c.sanitized()
}

func (c Config) sanitized() Config {
	if c.BaseDelay < 0 {
		c.BaseDelay = 0
	}
	if c.JitterMax < 0 {
		c.JitterMax = 0
	}
	if c.MaxWait < 0 {
		c.MaxWait = 0
	}
	if c.PollEvery < 0 {
		c.PollEvery = 0
	}
	if math.IsNaN(c.MaxLoad) || c.MaxLoad < 0 {
		c.MaxLoad = 0
	}
	return c
}

// Wait blocks according to cfg (see the package doc) and reports what it did.
// It returns ctx.Err() if ctx is cancelled first; a MaxWait timeout is not an
// error. It never panics and never blocks beyond BaseDelay + JitterMax +
// MaxWait.
func Wait(ctx context.Context, cfg Config) (Result, error) {
	cfg = cfg.sanitized()
	start := now()
	var res Result
	finish := func(err error) (Result, error) {
		res.Waited = now().Sub(start)
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}

	res.Jitter = jitterFor(cfg)
	if d := cfg.BaseDelay + res.Jitter; d > 0 {
		if err := sleep(ctx, d); err != nil {
			return finish(err)
		}
	}

	if cfg.MaxLoad <= 0 {
		return finish(nil)
	}
	maxWait := cfg.MaxWait
	if maxWait <= 0 {
		maxWait = DefaultMaxWait
	}
	poll := cfg.PollEvery
	if poll <= 0 {
		poll = DefaultPollEvery
	}

	gateStart := now()
	for first := true; ; first = false {
		load, ok := readLoad()
		if !ok {
			// Unknown load: never block on what we cannot measure.
			return finish(nil)
		}
		res.LoadKnown = true
		if first {
			res.LoadAtStart = load
		}
		res.LoadAtEnd = load
		if load < cfg.MaxLoad {
			return finish(nil)
		}
		remaining := maxWait - now().Sub(gateStart)
		if remaining <= 0 {
			res.TimedOut = true
			return finish(nil)
		}
		if err := sleep(ctx, min(poll, remaining)); err != nil {
			return finish(err)
		}
	}
}

// Go runs Wait and then fn in a new goroutine, logging one line with the
// Result. If ctx is cancelled during Wait, fn is not run.
func Go(ctx context.Context, cfg Config, fn func(context.Context)) {
	goDone(ctx, cfg, fn)
}

// goDone is Go, returning a channel closed when the goroutine finishes (so
// tests can synchronise with it).
func goDone(ctx context.Context, cfg Config, fn func(context.Context)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := Wait(ctx, cfg)
		if err != nil {
			log.Printf("bootgate: name=%q cancelled after %s: %v", cfg.Name, res.Waited, err)
			return
		}
		log.Printf("bootgate: name=%q waited=%s jitter=%s load_known=%t load_start=%.2f load_end=%.2f timed_out=%t",
			cfg.Name, res.Waited, res.Jitter, res.LoadKnown, res.LoadAtStart, res.LoadAtEnd, res.TimedOut)
		fn(ctx)
	}()
	return done
}

// jitterFor returns the deterministic jitter in [0, JitterMax) for cfg.
func jitterFor(cfg Config) time.Duration {
	if cfg.JitterMax <= 0 {
		return 0
	}
	seed := cfg.Seed
	if seed == "" {
		if h, err := hostname(); err == nil {
			seed = h
		}
	}
	if seed == "" {
		seed = cfg.Name
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	// FNV-1a alone mixes poorly into its high bits for short, similar
	// strings (hostnames, container IDs); a murmur3 finalizer avalanches it.
	return time.Duration(fmix64(h.Sum64()) % uint64(cfg.JitterMax))
}

func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func readHostLoad() (float64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	return parseLoadavg(string(b))
}

// parseLoadavg extracts the 1-minute load from /proc/loadavg content.
func parseLoadavg(s string) (float64, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

package egressproxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/baditaflorin/go-common/proxysupplier"
)

// Defaults applied by DefaultConfig and ConfigFromEnv.
const (
	DefaultMaxConns          = 256
	DefaultDialTimeout       = 10 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultReadHeaderTimeout = 15 * time.Second
	DefaultMaxTunnelBytes    = int64(64 << 20)
	DefaultGuardCacheTTL     = 30 * time.Second
	DefaultGuardCacheSize    = 4096
)

// Environment variables read by ConfigFromEnv.
const (
	EnvMaxConns          = "EGRESSPROXY_MAX_CONNS"
	EnvDialTimeout       = "EGRESSPROXY_DIAL_TIMEOUT"
	EnvIdleTimeout       = "EGRESSPROXY_IDLE_TIMEOUT"
	EnvReadHeaderTimeout = "EGRESSPROXY_READ_HEADER_TIMEOUT"
	EnvMaxTunnelBytes    = "EGRESSPROXY_MAX_TUNNEL_BYTES"
	EnvRequireUpstream   = "EGRESSPROXY_REQUIRE_UPSTREAM"
	EnvIgnoreUpstream5xx = "EGRESSPROXY_IGNORE_UPSTREAM_5XX"
	EnvGuardCacheTTL     = "EGRESSPROXY_GUARD_CACHE_TTL"
)

// Target describes one browser request the proxy is about to serve. It is
// handed to Config.KeyFunc. It never carries upstream credentials.
type Target struct {
	// Kind is "connect" for a CONNECT tunnel and "http" for a plain
	// absolute-URI request.
	Kind string
	// Host and Port are the destination the browser asked for (no brackets
	// around an IPv6 literal; Port defaults to 80 for plain HTTP).
	Host string
	Port string
	// Client is the address of the local connection (a loopback port of the
	// browser process).
	Client net.Addr
}

// Guard decides whether the proxy may open a path to host:port at all. A nil
// error allows it. An error that wraps ErrGuardUnavailable answers 502; any
// other error answers 403. The guard runs before the upstream proxy is
// dialed. See DefaultGuard.
type Guard func(ctx context.Context, host, port string) error

// Config configures Start. Start from DefaultConfig or ConfigFromEnv and set
// Supplier; a bare Config literal has no byte cap and does not require an
// upstream at start-up (zero means "off" for MaxTunnelBytes, as documented on
// each field). Durations and counts that cannot be zero (MaxConns, the
// timeouts, the guard cache) fall back to the defaults when they are <= 0.
type Config struct {
	// Supplier picks the upstream proxy per connection or request. It is the
	// only way out: there is no direct path to a target in this package.
	Supplier proxysupplier.Supplier

	// KeyFunc, when set and the Supplier also implements
	// proxysupplier.KeyedSupplier, gives the key passed to ProxyURLForKey (for
	// example the target host, to keep one exit IP per site). An empty key
	// means "no preference" and the supplier's ProxyURL is used. A retry always
	// uses ProxyURL: after a failure stickiness no longer matters.
	KeyFunc func(Target) string

	// RequireUpstream makes Start fail with ErrNoSupplier when Supplier is nil.
	// DefaultConfig and ConfigFromEnv set it to true. It does NOT enable any
	// direct path: whatever its value, a request for which no upstream is
	// available is refused with 503 and the target is never dialed.
	RequireUpstream bool

	// Guard is the SSRF check run on every target before the upstream is
	// dialed. Nil selects DefaultGuard. Its verdicts are cached per host:port
	// for GuardCacheTTL (denials as long as allows; "unavailable" verdicts
	// only briefly).
	Guard Guard

	// GuardCacheTTL and GuardCacheSize bound the verdict cache (defaults 30 s,
	// 4096 entries; the cache is cleared wholesale when full).
	GuardCacheTTL  time.Duration
	GuardCacheSize int

	// MaxConns caps concurrent client connections (default 256). A connection
	// over the cap is answered 503 immediately and closed.
	MaxConns int

	// DialTimeout bounds one request's upstream selection, dial and CONNECT
	// handshake across both attempts (default 10 s). The first attempt may use
	// at most 60% of it so that a hanging upstream still leaves time for the
	// retry.
	DialTimeout time.Duration

	// IdleTimeout closes a connection (and its upstream) when nothing moved in
	// either direction for this long (default 60 s).
	IdleTimeout time.Duration

	// ReadHeaderTimeout bounds reading one request's headers (default 15 s).
	ReadHeaderTimeout time.Duration

	// MaxTunnelBytes caps the bytes relayed in each direction of one tunnel
	// (and the request/response body of one plain HTTP exchange). Exceeding it
	// closes the tunnel and counts a byte_cap result. 0 or negative means
	// unlimited; DefaultConfig sets 64 MiB.
	MaxTunnelBytes int64

	// IgnoreUpstream5xx stops an upstream 5xx answer to CONNECT from counting
	// as an upstream failure: no MarkResult(false), no retry, the status is
	// relayed. By default a 5xx marks the exit IP bad, which for webshare_direct
	// starts a cooldown; set this when the upstream uses 5xx to report a dead
	// target rather than a bad exit IP.
	IgnoreUpstream5xx bool

	// Registerer receives the egressproxy_* metrics. Nil uses promx.Registry(),
	// the registry the service's /metrics serves once promx.Init (or
	// telemetry.Init) has run. A collector that is already registered (a second
	// Proxy in the same process) is reused.
	Registerer prometheus.Registerer

	// Logger receives operational logs (host:port of the upstream, scheme,
	// result; never the credentialed URL). Nil uses slog.Default().
	Logger *slog.Logger

	// Now is the clock for log rate limiting and the guard cache; nil uses
	// time.Now. Tests inject a fake.
	Now func() time.Time
}

// Errors returned by Start.
var (
	// ErrNoSupplier is returned by Start when RequireUpstream is set and
	// Config.Supplier is nil.
	ErrNoSupplier = errors.New("egressproxy: RequireUpstream is set but Config.Supplier is nil")
)

// DefaultConfig returns the documented defaults with RequireUpstream set. The
// caller still has to set Supplier.
func DefaultConfig() Config {
	return Config{
		RequireUpstream:   true,
		GuardCacheTTL:     DefaultGuardCacheTTL,
		GuardCacheSize:    DefaultGuardCacheSize,
		MaxConns:          DefaultMaxConns,
		DialTimeout:       DefaultDialTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		MaxTunnelBytes:    DefaultMaxTunnelBytes,
	}
}

// ConfigFromEnv returns DefaultConfig with environment overrides applied. It
// does not set Supplier, Guard, KeyFunc, Registerer or Logger. A value that
// does not parse (or is out of range) is ignored and the default stays.
//
//	EGRESSPROXY_MAX_CONNS             integer > 0
//	EGRESSPROXY_DIAL_TIMEOUT          duration (10s)
//	EGRESSPROXY_IDLE_TIMEOUT          duration (60s)
//	EGRESSPROXY_READ_HEADER_TIMEOUT   duration (15s)
//	EGRESSPROXY_MAX_TUNNEL_BYTES      integer bytes; 0 = unlimited
//	EGRESSPROXY_REQUIRE_UPSTREAM      bool (true)
//	EGRESSPROXY_IGNORE_UPSTREAM_5XX   bool (false)
//	EGRESSPROXY_GUARD_CACHE_TTL       duration (30s)
func ConfigFromEnv() Config {
	cfg := DefaultConfig()
	if v, ok := envInt(EnvMaxConns); ok && v > 0 {
		cfg.MaxConns = int(v)
	}
	if v, ok := envDuration(EnvDialTimeout); ok {
		cfg.DialTimeout = v
	}
	if v, ok := envDuration(EnvIdleTimeout); ok {
		cfg.IdleTimeout = v
	}
	if v, ok := envDuration(EnvReadHeaderTimeout); ok {
		cfg.ReadHeaderTimeout = v
	}
	if v, ok := envInt(EnvMaxTunnelBytes); ok && v >= 0 {
		cfg.MaxTunnelBytes = v
	}
	if v, ok := envBool(EnvRequireUpstream); ok {
		cfg.RequireUpstream = v
	}
	if v, ok := envBool(EnvIgnoreUpstream5xx); ok {
		cfg.IgnoreUpstream5xx = v
	}
	if v, ok := envDuration(EnvGuardCacheTTL); ok {
		cfg.GuardCacheTTL = v
	}
	return cfg
}

// withDefaults fills the fields that cannot sensibly be zero.
func (c Config) withDefaults() Config {
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.GuardCacheTTL <= 0 {
		c.GuardCacheTTL = DefaultGuardCacheTTL
	}
	if c.GuardCacheSize <= 0 {
		c.GuardCacheSize = DefaultGuardCacheSize
	}
	if c.MaxTunnelBytes < 0 {
		c.MaxTunnelBytes = 0
	}
	if c.Guard == nil {
		c.Guard = DefaultGuard
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

func envInt(name string) (int64, bool) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func envDuration(name string) (time.Duration, bool) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func envBool(name string) (bool, bool) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return false, false
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, false
	}
	return v, true
}

package spiffeauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/spiffe"
)

// Environment variable names. SPIFFE_ENDPOINT_SOCKET (the Workload API address)
// is the standard variable and is declared by package graph.
const (
	EnvMode          = "SPIFFEAUTH_MODE"
	EnvPeersFile     = "SPIFFEAUTH_PEERS_FILE"
	EnvPeers         = "SPIFFEAUTH_PEERS"
	EnvTrustDomains  = "SPIFFEAUTH_TRUST_DOMAINS"
	EnvJWTAudience   = "SPIFFEAUTH_JWT_AUDIENCE"
	EnvJWTMaxTTL     = "SPIFFEAUTH_JWT_MAX_TTL"
	EnvMTLSAddr      = "SPIFFEAUTH_MTLS_ADDR"
	EnvUnmapped      = "SPIFFEAUTH_UNMAPPED"
	EnvSocket        = "SPIFFE_ENDPOINT_SOCKET"
	DefaultTrustZone = "0exec.com"

	// DefaultJWTMaxTTL is the longest remaining lifetime of a JWT-SVID accepted
	// when SPIFFEAUTH_JWT_MAX_TTL is unset. SPIRE issues 5-minute JWT-SVIDs here;
	// the extra minute is clock skew.
	DefaultJWTMaxTTL = 6 * time.Minute
	// MaxJWTMaxTTL is the ceiling for SPIFFEAUTH_JWT_MAX_TTL.
	MaxJWTMaxTTL = 10 * time.Minute

	maxPeersFileBytes = 1 << 20
)

// Mode is the SPIFFEAUTH_MODE setting.
type Mode string

const (
	// ModeOff (the default): nothing is verified, no listener is started, no
	// Workload API connection is made.
	ModeOff Mode = "off"
	// ModeDual: a request carrying a valid, mapped SVID is authenticated as the
	// mapped principal; a request without one goes through the API key path
	// unchanged. Nothing is rejected for lacking a SPIFFE identity.
	ModeDual Mode = "dual"
)

// UnmappedPolicy is what happens to a presented SVID whose SPIFFE ID is not in
// the peer map.
type UnmappedPolicy string

const (
	// UnmappedDeny (the default): 403. Applies only to a caller that presented an SVID.
	UnmappedDeny UnmappedPolicy = "deny"
	// UnmappedFallback: count it and let the API key path decide.
	UnmappedFallback UnmappedPolicy = "fallback"
)

// Config configures an Authenticator. The zero value is ModeOff.
type Config struct {
	Mode Mode
	// Socket is the Workload API address, unix:///run/spire/agent.sock.
	Socket string
	// TrustDomains are the trust domains whose SVIDs are accepted (default 0exec.com).
	TrustDomains []string
	// Peers is the SPIFFE ID to principal map. An ID that is not in it is
	// refused per Unmapped, but only when the caller presented an SVID.
	Peers []Peer
	// JWTAudience lists the accepted JWT-SVID audiences; empty turns the JWT path off.
	JWTAudience []string
	// JWTMaxTTL is the longest remaining lifetime of an accepted JWT-SVID.
	JWTMaxTTL time.Duration
	// MTLSAddr is the listen address of the extra TLS listener (":8443"); empty = none.
	MTLSAddr string
	Unmapped UnmappedPolicy
	// ConnectTimeout bounds one wait for the first SVID in New (default 30 s).
	ConnectTimeout time.Duration
	// Observer receives failures that happen below the HTTP layer (a TLS
	// handshake that failed verification). server.New sets it to the promx
	// collectors. nil is fine.
	Observer middleware.IdentityObserver
}

// Enabled reports whether the configuration turns SPIFFE authentication on.
func (c Config) Enabled() bool { return c.Mode == ModeDual }

var (
	_ = env.Declare(env.Var{Name: EnvMode, Kind: env.KindString, Default: "off",
		Doc: "SPIFFE identity for incoming requests (package spiffeauth): off (default, no change) or dual (a request with a valid, mapped X.509-SVID or JWT-SVID is authenticated as the mapped principal; a request without one uses the API key as before). Roll back by unsetting it.",
		Check: func(v string) error {
			if _, err := parseMode(v); err != nil {
				return err
			}
			return nil
		}})
	_ = env.Declare(env.Var{Name: EnvPeersFile, Kind: env.KindString,
		Doc: `Path of a JSON file {"peers":[{"id":"spiffe://0exec.com/ns/*/sa/<service>","principal":"<service>","tier":"","scope":""}]} mapping SPIFFE IDs to principals. "*" is allowed only as the ns segment. Wins over SPIFFEAUTH_PEERS.`})
	_ = env.Declare(env.Var{Name: EnvPeers, Kind: env.KindString,
		Doc: "The same JSON as SPIFFEAUTH_PEERS_FILE, inline."})
	_ = env.Declare(env.Var{Name: EnvTrustDomains, Kind: env.KindList, Default: DefaultTrustZone,
		Doc: "Trust domains whose SVIDs spiffeauth accepts."})
	_ = env.Declare(env.Var{Name: EnvJWTAudience, Kind: env.KindList,
		Doc: "Accepted JWT-SVID audiences; empty turns the JWT-SVID path off."})
	_ = env.Declare(env.Var{Name: EnvJWTMaxTTL, Kind: env.KindDuration, Default: "6m",
		Doc: "Longest remaining lifetime of an accepted JWT-SVID (SPIRE issues 5 m), at most 10m."})
	_ = env.Declare(env.Var{Name: EnvMTLSAddr, Kind: env.KindString,
		Doc: "Listen address of the extra TLS listener that verifies client X.509-SVIDs, e.g. :8443. Empty: no mTLS listener (JWT-SVIDs still work on the normal port)."})
	_ = env.Declare(env.Var{Name: EnvUnmapped, Kind: env.KindString, Default: "deny",
		Doc: "A presented SVID whose SPIFFE ID is not in the peer map: deny (403, default) or fallback (count it and use the API key path).",
		Check: func(v string) error {
			if _, err := parseUnmapped(v); err != nil {
				return err
			}
			return nil
		}})
)

func parseMode(v string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "false", "0", "no", "disabled":
		return ModeOff, nil
	case "dual":
		return ModeDual, nil
	}
	return ModeOff, errors.New("want off or dual")
}

func parseUnmapped(v string) (UnmappedPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "deny":
		return UnmappedDeny, nil
	case "fallback":
		return UnmappedFallback, nil
	}
	return UnmappedDeny, errors.New("want deny or fallback")
}

// ConfigFromEnv reads the SPIFFEAUTH_* variables. With SPIFFEAUTH_MODE unset or
// off it returns a ModeOff Config and a nil error whatever else is set, so an
// unconfigured service is untouched. A configuration that cannot work (dual
// with no socket or no peers, a bad peer entry) is an error: the caller logs it
// and runs without the SPIFFE path (server.New does), never refusing to start.
func ConfigFromEnv() (Config, error) { return configFrom(os.Getenv) }

func configFrom(get func(string) string) (Config, error) {
	mode, err := parseMode(get(EnvMode))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvMode, err)
	}
	if mode == ModeOff {
		return Config{Mode: ModeOff}, nil
	}
	cfg := Config{Mode: mode, Socket: strings.TrimSpace(get(EnvSocket)), MTLSAddr: strings.TrimSpace(get(EnvMTLSAddr))}
	if cfg.Unmapped, err = parseUnmapped(get(EnvUnmapped)); err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvUnmapped, err)
	}
	cfg.TrustDomains = splitList(get(EnvTrustDomains))
	cfg.JWTAudience = splitList(get(EnvJWTAudience))
	if v := strings.TrimSpace(get(EnvJWTMaxTTL)); v != "" {
		if cfg.JWTMaxTTL, err = time.ParseDuration(v); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvJWTMaxTTL, err)
		}
	}
	raw := []byte(strings.TrimSpace(get(EnvPeers)))
	if path := strings.TrimSpace(get(EnvPeersFile)); path != "" {
		if raw, err = readPeersFile(path); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvPeersFile, err)
		}
	}
	if len(raw) > 0 {
		if cfg.Peers, err = ParsePeers(raw); err != nil {
			return Config{}, err
		}
	}
	return cfg, cfg.validate()
}

func readPeersFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("cannot read the peer map file")
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxPeersFileBytes {
		return nil, errors.New("the peer map must be a regular file of at most 1 MiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read the peer map file")
	}
	return b, nil
}

// ParsePeers decodes the peer map JSON: {"peers":[{"id","principal","tier","scope"}]}.
// Unknown fields are an error, so a typo does not silently drop a restriction.
func ParsePeers(b []byte) ([]Peer, error) {
	var doc struct {
		Peers []Peer `json:"peers"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.New("spiffeauth: the peer map is not valid JSON of the form {\"peers\":[...]}")
	}
	return doc.Peers, nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validate checks a ModeDual configuration and fills the defaults.
func (c *Config) validate() error {
	if c.Mode == ModeOff {
		return nil
	}
	if c.Mode != ModeDual {
		return fmt.Errorf("spiffeauth: unknown mode %q", c.Mode)
	}
	if !spiffe.ValidateSocket(c.Socket) {
		return fmt.Errorf("spiffeauth: %s must be a unix:///absolute/path Workload API address", EnvSocket)
	}
	if len(c.TrustDomains) == 0 {
		c.TrustDomains = []string{DefaultTrustZone}
	}
	if c.JWTMaxTTL == 0 {
		c.JWTMaxTTL = DefaultJWTMaxTTL
	}
	if c.JWTMaxTTL < time.Minute || c.JWTMaxTTL > MaxJWTMaxTTL {
		return fmt.Errorf("spiffeauth: %s must be between 1m and %s", EnvJWTMaxTTL, MaxJWTMaxTTL)
	}
	if c.Unmapped == "" {
		c.Unmapped = UnmappedDeny
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 30 * time.Second
	}
	if len(c.Peers) == 0 {
		return errors.New("spiffeauth: dual mode needs a peer map (SPIFFEAUTH_PEERS_FILE or SPIFFEAUTH_PEERS) with at least one peer")
	}
	for _, a := range c.JWTAudience {
		if a == "" || len(a) > 512 || strings.TrimSpace(a) != a {
			return errors.New("spiffeauth: invalid JWT audience")
		}
	}
	_, err := newPeerMap(c.Peers, c.TrustDomains)
	return err
}

package secrets

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The SPIFFE path for secret reads (phase 3 of the ephemeral-credentials program).
//
// With FLEET_SECRETS_SPIFFE=dual, FLEET_SECRETS_SPIFFE_URL and SPIFFE_ENDPOINT_SOCKET all set, Get first asks
// the vault's private mTLS listener (go-fleet-secrets with SPIFFEAUTH_MTLS_ADDR) and proves who it is with the
// workload's X.509-SVID. No API key travels on that request. DUAL MODE: the API key path stays exactly as it
// was and is the fallback. Any answer but a 200 (no connection, a refused handshake, 401, 403, 404, 429, 5xx, an
// undecodable body) sends the read on to the key path in the same Get, so nothing is lost; after a failure of the
// path itself (no connection, a refused handshake, 5xx) the next reads go straight to the key for a minute, so a
// vault whose listener is down costs nothing per read. A read the vault served over the key path to a caller that has
// a SPIFFE mapping is counted by the vault as spiffe_apikey_fallback_total: that is where "SPIFFE expected, key used"
// is measured.
//
// Package secrets does not speak the Workload API itself (go-spiffe, go-jose and gRPC would come with every
// importer: scripts/dep-budget.txt). It uses the client a SPIFFEClientFactory returns; server.New installs one built
// on package spiffe, and a service that does not use server.New calls SetSPIFFEClientFactory itself. Without a
// factory the variables have no effect.
//
//	FLEET_SECRETS_SPIFFE            off (default) or dual. Rollback: unset or off, restart; no release needed.
//	FLEET_SECRETS_SPIFFE_URL        https URL of the vault's mTLS listener, e.g. https://go-fleet-secrets:8443. Any host: the
//	                                server is authenticated by its SPIFFE ID, not by its name.
//	FLEET_SECRETS_SPIFFE_SERVER_ID  the only server SPIFFE ID accepted (default DefaultSPIFFEServerID)
//	SPIFFE_ENDPOINT_SOCKET          the Workload API, unix:///run/spire/sockets/agent.sock

const (
	envSPIFFEMode     = "FLEET_SECRETS_SPIFFE"
	envSPIFFEURL      = "FLEET_SECRETS_SPIFFE_URL"
	envSPIFFEServerID = "FLEET_SECRETS_SPIFFE_SERVER_ID"
	envSPIFFESocket   = "SPIFFE_ENDPOINT_SOCKET"

	// DefaultSPIFFEServerID is the SPIFFE ID of the vault.
	DefaultSPIFFEServerID = "spiffe://0exec.com/ns/fleet/sa/fleet-secrets"

	// SourceSPIFFE: the value came over the vault's mTLS listener, with no API key.
	SourceSPIFFE Source = "spiffe"

	spiffeConnectTimeout  = 30 * time.Second
	spiffeConnectWait     = 1500 * time.Millisecond // how long a read waits for the first SVID
	spiffeRequestTimeout  = 3 * time.Second
	spiffeFailureCooldown = time.Minute
	spiffeRetryMin        = 5 * time.Second
	spiffeRetryMax        = 5 * time.Minute
)

// SPIFFEClient is what the client needs from a SPIFFE mTLS HTTP client. *spiffe.HTTPClient satisfies it.
type SPIFFEClient interface {
	Do(*http.Request) (*http.Response, error)
	Close() error
}

// SPIFFEClientFactory connects to the Workload API at socket and returns a client that presents this workload's
// X.509-SVID and accepts only a server that proves one of allowedServerIDs, with timeout as its total request
// timeout. It may block until the first SVID arrives or ctx ends.
type SPIFFEClientFactory func(ctx context.Context, socket string, allowedServerIDs []string, timeout time.Duration) (SPIFFEClient, error)

var (
	spiffeFactoryMu sync.Mutex
	spiffeFactory   SPIFFEClientFactory

	spiffeLegsMu sync.Mutex
	spiffeLegs   = map[string]*spiffeLeg{}

	spiffeReads    atomic.Int64
	spiffeFallback atomic.Int64
)

// SetSPIFFEClientFactory installs the factory the SPIFFE read path uses. The first non-nil factory wins and later
// calls are ignored, so a service that installs its own before server.New keeps it.
func SetSPIFFEClientFactory(f SPIFFEClientFactory) {
	if f == nil {
		return
	}
	spiffeFactoryMu.Lock()
	defer spiffeFactoryMu.Unlock()
	if spiffeFactory == nil {
		spiffeFactory = f
	}
}

func currentSPIFFEFactory() SPIFFEClientFactory {
	spiffeFactoryMu.Lock()
	defer spiffeFactoryMu.Unlock()
	return spiffeFactory
}

// SPIFFEStats returns how many reads this process served over SPIFFE and how many it sent on to the API key after
// trying SPIFFE. Counters only, no names and no values.
func SPIFFEStats() (served, fellBack int64) { return spiffeReads.Load(), spiffeFallback.Load() }

// WithSPIFFE enables the SPIFFE path to the vault listener at baseURL for this client. serverID == "" means
// DefaultSPIFFEServerID. An invalid URL or ID leaves the client on the API key and returns an error (WithEnv
// logs it and carries on).
func (c *Client) WithSPIFFE(baseURL, serverID, socket string) error {
	base, err := normalizeSPIFFEURL(baseURL)
	if err != nil {
		return err
	}
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		serverID = DefaultSPIFFEServerID
	}
	if !validSPIFFEIDShape(serverID) {
		return errors.New("secrets: SPIFFE server ID is not a spiffe:// ID")
	}
	socket = strings.TrimSpace(socket)
	if !strings.HasPrefix(socket, "unix://") || len(socket) == len("unix://") {
		return errors.New("secrets: SPIFFE_ENDPOINT_SOCKET must be a unix:// address")
	}
	c.sp = sharedSPIFFELeg(base, serverID, socket)
	return nil
}

// withSPIFFEEnv applies the FLEET_SECRETS_SPIFFE* variables: a no-op unless the mode is dual.
func (c *Client) withSPIFFEEnv(getenv func(string) string) {
	switch strings.ToLower(strings.TrimSpace(getenv(envSPIFFEMode))) {
	case "dual":
	default:
		return
	}
	if err := c.WithSPIFFE(getenv(envSPIFFEURL), getenv(envSPIFFEServerID), getenv(envSPIFFESocket)); err != nil {
		log.Printf("secrets: SPIFFE read path not enabled, using the API key: %v", err)
	}
}

func normalizeSPIFFEURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("secrets: FLEET_SECRETS_SPIFFE_URL is empty")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", errors.New("secrets: SPIFFE URL is not valid")
	case !strings.EqualFold(u.Scheme, "https"):
		return "", errors.New("secrets: SPIFFE URL must be https")
	case u.Hostname() == "":
		return "", errors.New("secrets: SPIFFE URL needs a host")
	case u.User != nil:
		return "", errors.New("secrets: SPIFFE URL must not carry credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		return "", errors.New("secrets: SPIFFE URL must be a bare https origin")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func validSPIFFEIDShape(id string) bool {
	return strings.HasPrefix(id, "spiffe://") && len(id) > len("spiffe://") && len(id) <= 2048 &&
		!strings.ContainsAny(id, " \t\r\n?#")
}

// spiffeLeg is the process-wide connection to the Workload API and the vault listener for one (url, id, socket).
type spiffeLeg struct {
	base, serverID, socket string

	mu         sync.Mutex
	client     SPIFFEClient
	attempt    chan struct{} // non-nil while a connect is in flight; closed when it ends
	retryAt    time.Time
	pause      time.Duration
	attempts   int
	coolUntil  time.Time
	lastNotice time.Time
}

func sharedSPIFFELeg(base, serverID, socket string) *spiffeLeg {
	key := base + "\x00" + serverID + "\x00" + socket
	spiffeLegsMu.Lock()
	defer spiffeLegsMu.Unlock()
	l := spiffeLegs[key]
	if l == nil {
		l = &spiffeLeg{base: base, serverID: serverID, socket: socket, pause: spiffeRetryMin}
		spiffeLegs[key] = l
	}
	return l
}

// get returns the connected client, waiting briefly for the first connect, or nil when the path is not usable now.
func (l *spiffeLeg) get(ctx context.Context) SPIFFEClient {
	factory := currentSPIFFEFactory()
	if factory == nil {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if now.Before(l.coolUntil) {
		l.mu.Unlock()
		return nil
	}
	if l.client != nil {
		c := l.client
		l.mu.Unlock()
		return c
	}
	if l.attempt == nil && !now.Before(l.retryAt) {
		l.attempt = make(chan struct{})
		go l.connect(factory, l.attempt)
	}
	wait := l.attempt
	l.mu.Unlock()
	if wait == nil {
		return nil
	}
	t := time.NewTimer(spiffeConnectWait)
	defer t.Stop()
	select {
	case <-wait:
	case <-t.C:
	case <-ctx.Done():
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.client
}

func (l *spiffeLeg) connect(factory SPIFFEClientFactory, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), spiffeConnectTimeout)
	client, err := factory(ctx, l.socket, []string{l.serverID}, spiffeRequestTimeout)
	cancel()
	l.mu.Lock()
	defer func() {
		l.attempt = nil
		l.mu.Unlock()
		close(done)
	}()
	l.attempts++
	if err == nil && client != nil {
		l.client, l.pause = client, spiffeRetryMin
		return
	}
	if l.attempts == 1 || l.attempts%10 == 0 {
		log.Printf("secrets: SPIFFE path not available yet (attempt %d, retry in %s), reads use the API key: %v", l.attempts, l.pause, err)
	}
	l.retryAt = time.Now().Add(l.pause)
	if l.pause *= 2; l.pause > spiffeRetryMax {
		l.pause = spiffeRetryMax
	}
}

// failed starts the cool-down after a failure of the path itself.
func (l *spiffeLeg) failed(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.coolUntil = now.Add(spiffeFailureCooldown)
	if now.Sub(l.lastNotice) >= spiffeFailureCooldown {
		l.lastNotice = now
		log.Printf("secrets: SPIFFE path failed (%s), reads use the API key for %s", reason, spiffeFailureCooldown)
	}
}

// refused logs, at most once a minute, that the vault answered the SPIFFE request with a refusal.
func (l *spiffeLeg) refused(status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.lastNotice) >= spiffeFailureCooldown {
		l.lastNotice = now
		log.Printf("secrets: the vault answered the SPIFFE read with status %d, using the API key", status)
	}
}

// trySPIFFE reads name over the SPIFFE path. done is false when the read must go the API key path. A 404 is
// final (done with err): the vault answered for the same principal, so the key would hear the same, and a secret
// that does not exist (a probe for an optional credential) must not make every start a "fallback to the key".
func (c *Client) trySPIFFE(ctx context.Context, name string) (value string, done bool, err error) {
	if c.sp == nil {
		return "", false, nil
	}
	client := c.sp.get(ctx)
	if client == nil {
		spiffeFallback.Add(1)
		return "", false, nil
	}
	v, err := c.fetchVia(ctx, client, c.sp.base, name, "")
	if err == nil {
		spiffeReads.Add(1)
		return v, true, nil
	}
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
		spiffeReads.Add(1)
		return "", true, err
	}
	spiffeFallback.Add(1)
	switch {
	case errors.As(err, &se) && se.StatusCode < 500 && se.StatusCode != http.StatusTooManyRequests:
		c.sp.refused(se.StatusCode)
	case errors.As(err, &se):
		c.sp.failed("status " + http.StatusText(se.StatusCode))
	default:
		c.sp.failed("transport")
	}
	return "", false, nil
}

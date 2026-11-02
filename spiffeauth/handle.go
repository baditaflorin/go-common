package spiffeauth

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/middleware"
)

// Handle is an Authenticator that connects to the Workload API in the
// background. A service never waits for SPIRE to start: until the connection is
// up the Handle authenticates nothing (every request goes the API key path) and
// its mTLS listener refuses client handshakes it cannot verify. It implements
// middleware.IdentityAuthenticator.
type Handle struct {
	cfg    Config
	peers  *peerMap
	logf   func(string, ...any)
	auth   atomic.Pointer[Authenticator]
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

var _ middleware.IdentityAuthenticator = (*Handle)(nil)

// Start validates cfg and, for an enabled one, connects in the background,
// retrying with a growing pause until it succeeds. It returns nil, nil for a
// disabled (ModeOff) configuration, which is the inert default, and an error for
// a configuration that cannot work. logf (may be nil) gets one line per state
// change and never a credential.
func Start(cfg Config, logf func(string, ...any)) (*Handle, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	pm, err := newPeerMap(cfg.Peers, cfg.TrustDomains)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handle{cfg: cfg, peers: pm, logf: logf, cancel: cancel, done: make(chan struct{})}
	go h.connect(ctx)
	return h, nil
}

func (h *Handle) connect(ctx context.Context) {
	defer close(h.done)
	pause := 5 * time.Second
	for attempt := 1; ; attempt++ {
		a, err := New(ctx, h.cfg)
		if err == nil {
			h.auth.Store(a)
			h.logf("spiffeauth: dual mode ready (%d peers, jwt=%t, mtls_listener=%t)", len(h.cfg.Peers), len(h.cfg.JWTAudience) > 0, h.cfg.MTLSAddr != "")
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			h.logf("spiffeauth: waiting for the Workload API (attempt %d): %v", attempt, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		if pause < 5*time.Minute {
			pause *= 2
		}
	}
}

// Ready reports whether the Workload API connection is up.
func (h *Handle) Ready() bool { return h != nil && h.auth.Load() != nil }

// Config returns the configuration in use.
func (h *Handle) Config() Config { return h.cfg }

// AuthenticateIdentity implements middleware.IdentityAuthenticator: it
// authenticates nothing until the connection is up.
func (h *Handle) AuthenticateIdentity(r *http.Request) middleware.IdentityDecision {
	if h == nil {
		return middleware.IdentityDecision{}
	}
	if a := h.auth.Load(); a != nil {
		return a.AuthenticateIdentity(r)
	}
	return middleware.IdentityDecision{}
}

// ExpectedPrincipal implements middleware.IdentityAuthenticator; it needs only
// the configured map, so it works before the connection is up.
func (h *Handle) ExpectedPrincipal(user string) (string, bool) {
	if h == nil {
		return "", false
	}
	return h.peers.expected(user)
}

// StrictPresented implements middleware.StrictIdentity; like ExpectedPrincipal it
// needs only the configuration.
func (h *Handle) StrictPresented() bool { return h != nil && h.cfg.StrictPresented }

// TLSConfig is the tls.Config for the mTLS listener. It asks the current
// Authenticator for its config on every handshake, so it works once the
// connection is up and refuses handshakes before.
func (h *Handle) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			a := h.auth.Load()
			if a == nil {
				if o := h.cfg.Observer; o != nil {
					o.ObserveIdentityFailure(middleware.AuthSourceSPIFFEMTLS, ReasonNotReady)
				}
				return nil, errors.New("spiffeauth: not ready")
			}
			return a.TLSConfig()
		},
	}
}

// Close stops the background connect and releases the connections. It is safe to call twice.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	var err error
	h.once.Do(func() {
		h.cancel()
		<-h.done
		if a := h.auth.Load(); a != nil {
			err = a.Close()
		}
	})
	return err
}

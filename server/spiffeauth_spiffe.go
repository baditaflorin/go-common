//go:build !gocommon_nospiffe

package server

import (
	"crypto/tls"

	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/spiffeauth"
)

// startSPIFFEAuth reads SPIFFEAUTH_* and starts the authenticator. nil when the
// mode is off or the configuration cannot work (logged). A binary built with
// -tags gocommon_nospiffe does not link package spiffeauth: see spiffeauth_nospiffe.go.
func startSPIFFEAuth(obs middleware.IdentityObserver, logf func(string, ...any)) *identityRuntime {
	cfg, err := spiffeauth.ConfigFromEnv()
	if err != nil {
		logf("server: SPIFFE identity not enabled, running with API keys only: %v", err)
		return nil
	}
	if !cfg.Enabled() {
		return nil
	}
	cfg.Observer = obs
	h, err := spiffeauth.Start(cfg, logf)
	if err != nil || h == nil {
		if err != nil {
			logf("server: SPIFFE identity not enabled, running with API keys only: %v", err)
		}
		return nil
	}
	return &identityRuntime{
		auth:      h,
		tlsConfig: func() *tls.Config { return h.TLSConfig() },
		addr:      cfg.MTLSAddr,
		close:     h.Close,
	}
}

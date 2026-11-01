package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/middleware"
)

// The SPIFFE identity path (package spiffeauth), wired from the environment.
//
// With SPIFFEAUTH_MODE unset or off nothing here runs: no Workload API
// connection, no listener, no metric series, the same middleware as before.
// With SPIFFEAUTH_MODE=dual the keystore-auth middleware also accepts a request
// carrying a valid, mapped SVID or JWT-SVID, and SPIFFEAUTH_MTLS_ADDR adds a TLS
// listener that verifies client certificates; a request with no SPIFFE identity
// goes the API key path exactly as before. A misconfiguration is logged and the
// service runs without the SPIFFE path: it never stops a service from starting.

// identityRuntime is the process-wide SPIFFE authenticator and its listener settings.
type identityRuntime struct {
	auth      middleware.IdentityAuthenticator
	tlsConfig func() *tls.Config
	addr      string
	close     func() error
}

var (
	identityOnce sync.Once
	identityRT   *identityRuntime
)

// spiffeRuntime starts the process-wide authenticator once (the first server.New
// that reaches a keystore option) and returns it, or nil when the path is off.
func (s *Server) spiffeRuntime() *identityRuntime {
	identityOnce.Do(func() {
		var obs middleware.IdentityObserver
		if s.PromAuthCollectors != nil {
			obs = s.PromAuthCollectors
		}
		identityRT = startSPIFFEAuth(obs, log.Printf)
	})
	return identityRT
}

// spiffeIdentity is what the KeystoreOpts built by this package carry: nil
// (an untyped nil interface) when the SPIFFE path is off.
func (s *Server) spiffeIdentity() middleware.IdentityAuthenticator {
	if rt := s.spiffeRuntime(); rt != nil {
		return rt.auth
	}
	return nil
}

// startSPIFFEListener serves h on SPIFFEAUTH_MTLS_ADDR when it is set. It returns
// the function that stops it. A listen failure is logged, never fatal.
func (s *Server) startSPIFFEListener(h http.Handler) (stop func()) {
	rt := s.spiffeRuntime()
	if rt == nil || rt.addr == "" || rt.tlsConfig == nil {
		return func() {}
	}
	srv := s.buildHTTPServer(rt.addr, h)
	srv.TLSConfig = rt.tlsConfig()
	go func() {
		log.Printf("server: SPIFFE mTLS listener on %s", rt.addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: SPIFFE mTLS listener stopped: %v", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

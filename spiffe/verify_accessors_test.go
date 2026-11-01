package spiffe

import (
	"crypto"
	"testing"

	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// bundleJWTSource is a jwtSource that is also a jwtbundle.Source, like *workloadapi.JWTSource.
type bundleJWTSource struct {
	fakeJWTSource
	set *jwtbundle.Set
}

func (b bundleJWTSource) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return b.set.GetJWTBundleForTrustDomain(td)
}

func TestSourceIsAJWTBundleSourceWhenItsClientIs(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("0exec.com")
	set := jwtbundle.NewSet(jwtbundle.FromJWTAuthorities(td, map[string]crypto.PublicKey{}))
	s := &Source{source: bundleJWTSource{set: set}}
	if b, err := s.GetJWTBundleForTrustDomain(td); err != nil || b == nil {
		t.Fatalf("bundle = %v, %v", b, err)
	}
	other := spiffeid.RequireTrustDomainFromString("other.example")
	if _, err := s.GetJWTBundleForTrustDomain(other); err == nil {
		t.Fatal("a trust domain with no bundle must be an error")
	}
	// A source that cannot serve bundles, a nil Source: unavailable, never a panic.
	if _, err := (&Source{source: fakeJWTSource{}}).GetJWTBundleForTrustDomain(td); err != ErrUnavailable {
		t.Fatalf("err = %v", err)
	}
	if _, err := (*Source)(nil).GetJWTBundleForTrustDomain(td); err != ErrUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestIdentityAccessorsAreNilSafe(t *testing.T) {
	var nilID *X509Identity
	if nilID.SVIDSource() != nil || nilID.BundleSource() != nil {
		t.Fatal("a nil identity has no sources")
	}
	empty := &X509Identity{}
	if empty.SVIDSource() != nil || empty.BundleSource() != nil {
		t.Fatal("an identity without a source has no sources")
	}
}

func TestValidateSocket(t *testing.T) {
	for _, s := range []string{"unix:///run/spire/agent.sock", "  unix:///run/spire/agent.sock "} {
		if !ValidateSocket(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", "tcp://127.0.0.1:8081", "unix://relative.sock", "/run/spire/agent.sock", "unix:///a?x=1"} {
		if ValidateSocket(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

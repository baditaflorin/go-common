package spiffeauth

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const maxPeers = 1024

// Peer maps one SPIFFE ID (or one service in either namespace) to the principal
// a request carrying it is authenticated as.
type Peer struct {
	// ID is spiffe://<trust domain>/ns/<ns>/sa/<service>. "*" may stand for the
	// whole ns segment (spiffe://0exec.com/ns/*/sa/agent-tools covers
	// /ns/fleet/sa/agent-tools and /ns/0crawl/sa/agent-tools, and no other
	// namespace). An exact entry wins over a wildcard one.
	ID string `json:"id"`
	// Principal is what handlers see in X-Auth-User and what the graph labels
	// the caller with: use the service's name (lowercase, with a "-"). When the
	// service also has API keys, use the principal the keystore returns for them,
	// which is what lets spiffe_apikey_fallback_total see a fallback.
	Principal string `json:"principal"`
	// Tier is the access tier for KeystoreOpts.RequiredTier ("" = none, like a pre-tiering key).
	Tier string `json:"tier,omitempty"`
	// Scope is what X-Auth-Scope carries for this peer.
	Scope string `json:"scope,omitempty"`
}

// wildcardNamespace: the namespaces an "ns/*" entry covers, the two the fleet
// issues: "fleet" (0exec workloads) and "0crawl" (the merged 0crawl workloads).
func wildcardNamespace(ns string) bool { return ns == "fleet" || ns == "0crawl" }

// ParseWorkloadID splits spiffe://<trust domain>/ns/<ns>/sa/<service>, the
// shape of every workload identity in the fleet (the 0exec namespaces are
// "fleet" and "0crawl"). ok is false for any other path.
func ParseWorkloadID(id string) (trustDomain, namespace, service string, ok bool) {
	rest, found := strings.CutPrefix(id, "spiffe://")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || parts[0] == "" || parts[1] != "ns" || parts[3] != "sa" || parts[2] == "" || parts[4] == "" {
		return "", "", "", false
	}
	return parts[0], parts[2], parts[4], true
}

type peerKey struct{ td, sa string }

type peerMap struct {
	exact      map[string]Peer
	wild       map[peerKey]Peer
	principals map[string]struct{}
}

func newPeerMap(peers []Peer, trustDomains []string) (*peerMap, error) {
	if len(peers) == 0 || len(peers) > maxPeers {
		return nil, fmt.Errorf("spiffeauth: the peer map needs between 1 and %d peers", maxPeers)
	}
	domains := map[string]struct{}{}
	for _, d := range trustDomains {
		td, err := spiffeid.TrustDomainFromString(d)
		if err != nil {
			return nil, errors.New("spiffeauth: invalid trust domain")
		}
		domains[td.Name()] = struct{}{}
	}
	m := &peerMap{exact: map[string]Peer{}, wild: map[peerKey]Peer{}, principals: map[string]struct{}{}}
	for i, p := range peers {
		if !validLabel(p.Principal) || len(p.Tier) > 64 || len(p.Scope) > 128 || strings.ContainsAny(p.Tier+p.Scope, "\r\n") {
			return nil, fmt.Errorf("spiffeauth: peer %d: principal, tier or scope is not valid", i)
		}
		td, ns, sa, ok := ParseWorkloadID(p.ID)
		if !ok || strings.TrimSpace(p.ID) != p.ID {
			return nil, fmt.Errorf("spiffeauth: peer %d: id must be spiffe://<trust domain>/ns/<ns>/sa/<service>", i)
		}
		if _, ok := domains[td]; !ok {
			return nil, fmt.Errorf("spiffeauth: peer %d: trust domain is not in %s", i, EnvTrustDomains)
		}
		if ns == "*" {
			k := peerKey{td, sa}
			if _, dup := m.wild[k]; dup {
				return nil, fmt.Errorf("spiffeauth: peer %d: duplicate id", i)
			}
			if _, err := spiffeid.FromString("spiffe://" + td + "/ns/x/sa/" + sa); err != nil {
				return nil, fmt.Errorf("spiffeauth: peer %d: invalid id", i)
			}
			m.wild[k] = p
		} else {
			id, err := spiffeid.FromString(p.ID)
			if err != nil || id.String() != p.ID {
				return nil, fmt.Errorf("spiffeauth: peer %d: invalid id", i)
			}
			if _, dup := m.exact[p.ID]; dup {
				return nil, fmt.Errorf("spiffeauth: peer %d: duplicate id", i)
			}
			m.exact[p.ID] = p
		}
		m.principals[p.Principal] = struct{}{}
	}
	return m, nil
}

func (m *peerMap) lookup(id spiffeid.ID) (Peer, bool) {
	if p, ok := m.exact[id.String()]; ok {
		return p, true
	}
	td, ns, sa, ok := ParseWorkloadID(id.String())
	if !ok || !wildcardNamespace(ns) {
		return Peer{}, false
	}
	p, ok := m.wild[peerKey{td, sa}]
	return p, ok
}

// expected returns the configured principal string equal to user.
func (m *peerMap) expected(user string) (string, bool) {
	if _, ok := m.principals[user]; ok {
		return user, true
	}
	return "", false
}

// validLabel: 1-64 of lowercase letters, digits, '.', '_', '-'.
func validLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

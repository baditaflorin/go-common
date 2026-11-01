// Package spiffeauth authenticates incoming requests by SPIFFE identity (an X.509-SVID over mTLS or an audience-scoped JWT-SVID) next to the API key, in dual mode: a request without a SPIFFE identity is handled exactly as before.
//
// It is the server side of the ephemeral-credentials program (fleet-state
// docs/architecture/ephemeral-credentials, Phase 3): callers that hold a SPIRE
// identity stop depending on a long-lived API key, while every caller that still
// sends a key keeps working. It ships inert: nothing is verified, listened on or
// connected to until SPIFFEAUTH_MODE=dual is set.
//
// # What a service does
//
// Nothing in code. server.New reads the SPIFFEAUTH_* variables and, when the mode
// is dual, hands an Authenticator to every WithKeystoreAuth* option
// (middleware.KeystoreOpts.Identity). A service that builds its own
// middleware.TokenAuthKeystore sets Identity itself:
//
//	h, err := spiffeauth.Start(cfg, log.Printf) // cfg from spiffeauth.ConfigFromEnv()
//	...
//	middleware.TokenAuthKeystore(middleware.KeystoreOpts{Verifier: ks, Observer: auth, Identity: h})
//
// # Configuration
//
//	SPIFFEAUTH_MODE          off (default) | dual. Rollback: unset it and restart.
//	SPIFFE_ENDPOINT_SOCKET   unix:///run/spire/agent.sock (the Workload API)
//	SPIFFEAUTH_PEERS_FILE    JSON peer map (or SPIFFEAUTH_PEERS inline), see below
//	SPIFFEAUTH_TRUST_DOMAINS comma list, default 0exec.com
//	SPIFFEAUTH_JWT_AUDIENCE  comma list; empty = no JWT-SVID path
//	SPIFFEAUTH_JWT_MAX_TTL   default 6m, at most 10m (SPIRE issues 5m)
//	SPIFFEAUTH_MTLS_ADDR     :8443 = extra TLS listener that verifies client certificates
//	SPIFFEAUTH_UNMAPPED      deny (default) | fallback
//
// The peer map says which SPIFFE ID is which principal:
//
//	{"peers":[
//	  {"id":"spiffe://0exec.com/ns/*/sa/agent-tools","principal":"agent-tools","tier":"internal","scope":"write"},
//	  {"id":"spiffe://0exec.com/ns/0crawl/sa/go-url-fetcher","principal":"go-url-fetcher"}
//	]}
//
// Both ID shapes the fleet issues are understood, spiffe://0exec.com/ns/fleet/sa/<service>
// and spiffe://0exec.com/ns/0crawl/sa/<service>; "*" as the ns segment covers
// those two namespaces and no other (ParseWorkloadID). The principal, tier and
// scope are what the request carries afterwards (X-Auth-User, X-Auth-Tier,
// X-Auth-Scope, the graph caller label, and the RequiredTier gate), replacing
// anything the caller sent in those headers.
//
// # What counts as an identity
//
//   - mTLS: a client certificate on the SPIFFEAUTH_MTLS_ADDR listener. The
//     handshake verifies it against the trust bundle (an invalid or foreign one
//     fails the handshake), and the request check verifies it again.
//   - JWT-SVID: Authorization: Bearer <jwt> (only a JWT-shaped value, never an
//     ak_/fb_ key) or the X-Spiffe-Jwt header, on any listener. Signature,
//     expiry and audience are checked against the bundle, and a token with more
//     than SPIFFEAUTH_JWT_MAX_TTL left is refused, so a leaked token dies in
//     minutes. A JWT-SVID is never sent to the keystore as if it were a key.
//
// # Dual mode
//
// A request with no SVID is not touched: gateway header, local token and keystore
// work as before, and nothing is rejected for lacking a SPIFFE identity. The
// default-deny applies only to a caller that presented an SVID: an ID missing
// from the map gets 403 (SPIFFEAUTH_UNMAPPED=fallback counts it and lets the key
// decide). A presented JWT-SVID that does not verify is counted and, when the
// request also carries a key, the key decides; without a key the answer is 401.
//
// # Metrics
//
//	apikey_auth_total{source="spiffe-mtls"|"spiffe-jwt", result}
//	spiffe_verify_failures_total{source, reason}
//	spiffe_apikey_fallback_total{principal, reason}
//
// The last one counts an API key authenticating a principal that has a SPIFFE
// mapping, "SPIFFE expected, key used". The Phase 3 exit metric, fallback below
// 0.1 %:
//
//	sum(rate(spiffe_apikey_fallback_total[7d])) /
//	  (sum(rate(spiffe_apikey_fallback_total[7d])) +
//	   sum(rate(apikey_auth_total{source=~"spiffe-.*",result="allow"}[7d])))
//
// No token, certificate or peer detail reaches a log line, an error or a metric
// label: the labels are the fixed Reason* tokens and operator-configured principals.
//
// A binary built with -tags gocommon_nospiffe does not link this package or
// go-spiffe; server.New then only logs that SPIFFEAUTH_MODE has no effect.
package spiffeauth

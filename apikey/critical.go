// critical.go — fail-fast caller-key resolution for critical infra.
//
// Sister to resolve.go: the permissive Resolve API lets callers
// decide whether to fall back to the public demo "default_token".
// For consumer-tier services that read open data, that's fine. For
// critical infra (DNS reconciler, vault clients, deploy gates),
// falling back to default_token means the service identifies as
// actor=demo at the keystore — which is never on the consumers
// allowlist for production secrets like hcloud_token. Every vault
// read 403s ("not in consumers list"), the service's background
// ticker silently logs zero successful runs, and operators end up
// doing manual API calls.
//
// The 2026-05-17 incident that motivated this: go-fleet-dns-sync
// shipped with FLEET_API_KEY=default_token fallback in its compose
// file. /health was green for >24h while the 30-min reconcile
// ticker logged 0 syncs because every vault token-fetch returned
// 403. New services' A records didn't auto-provision; an operator
// POSTed to Hetzner Cloud Zones API by hand to unblock a bootstrap.
//
// ResolveCritical / MustResolveCritical replace the silent-failure-
// on-misconfig path with a structured error (or loud fatal) at
// boot. Pair with a docker-compose.yml that uses ${FLEET_API_KEY:?…}
// so the container won't even start without an explicit value.
//
// Error / fatal format is deterministic so an AI agent reading
// container logs can regex out slug, reason, and the literal
// remediation command:
//
//	apikey.critical_key_missing slug=<slug> env=<varname> reason=<reason> fix=`<cmd>` docs=<url>

package apikey

import (
	"fmt"
	"log"
	"strings"
)

// CriticalRunbookURL is the canonical doc anchor cited in every
// critical-key fatal / error. Exposed so callers (and tests) can
// reference it without string-duplication.
const CriticalRunbookURL = "https://github.com/baditaflorin/services-registry/blob/main/RUNBOOK-UNATTENDED.md#service-principals"

// demoTokenLiteral is the public demo key value. Hardcoded here for
// the *negative* check only — we refuse it. Never used as a fallback.
const demoTokenLiteral = "default_token"

// ResolveCritical walks envVars in order and returns the first
// non-empty value, or a structured error if the chain is unset, the
// chosen value is the public demo "default_token", or the chosen
// value carries no recognised fleet prefix.
//
// slug is the service.yaml `id:` — it appears in the error so an
// operator or AI agent can copy-paste the exact remediation:
//
//	fleet-runner key issue <slug> --never-expires
//	# then set FLEET_API_KEY=<returned-key> in /opt/services/<slug>/.env
//	# on the dockerhost and rolling-restart.
//
// On success, returns the resolved key. NEVER log the returned value.
func ResolveCritical(slug string, envVars ...string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("apikey.ResolveCritical: empty slug (programmer error, not config)")
	}
	if len(envVars) == 0 {
		return "", fmt.Errorf("apikey.ResolveCritical: empty envVars list (programmer error, not config)")
	}
	r := Resolve(envVars...)
	if !r.Found {
		return "", criticalErr(slug, strings.Join(envVars, ","), "unset",
			fmt.Sprintf("fleet-runner key issue %s --never-expires; install returned value as FLEET_API_KEY on dockerhost (/opt/services/%s/.env); docker compose up -d", slug, slug))
	}
	if r.Key == demoTokenLiteral {
		return "", criticalErr(slug, r.Source, "demo_default_token",
			fmt.Sprintf("fleet-runner key issue %s --never-expires; install returned value as FLEET_API_KEY on dockerhost (/opt/services/%s/.env); docker compose up -d", slug, slug))
	}
	if !HasFleetPrefix(r.Key) {
		return "", criticalErr(slug, r.Source,
			fmt.Sprintf("unknown_prefix (expected %s* or %s*)", KeyPrefixDynamic, KeyPrefixFallback),
			fmt.Sprintf("fleet-runner key issue %s --never-expires; install returned value as FLEET_API_KEY", slug))
	}
	return r.Key, nil
}

// MustResolveCritical is the fatal-on-error wrapper around
// ResolveCritical. Standard usage at service startup:
//
//	cfg.FleetAPIKey = apikey.MustResolveCritical("go-fleet-dns-sync", "FLEET_API_KEY")
//
// On misconfig, log.Fatalf exits 1 before the service ever opens its
// listener. The fatal line carries the same structured shape as the
// error returned by ResolveCritical, so logs are uniformly parseable.
func MustResolveCritical(slug string, envVars ...string) string {
	k, err := ResolveCritical(slug, envVars...)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return k
}

// criticalErr renders the canonical structured-error shape. Single
// helper so the format is impossible to drift between call sites.
func criticalErr(slug, env, reason, fix string) error {
	return fmt.Errorf("apikey.critical_key_missing slug=%s env=%s reason=%s fix=`%s` docs=%s",
		slug, env, reason, fix, CriticalRunbookURL)
}

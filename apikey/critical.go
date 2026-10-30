// critical.go — fail-fast caller-key resolution for critical infrastructure.
// ResolveCritical refuses missing, unrecognized, and retired shared fallback
// credentials so Vault allowlists receive only a real service principal.
// Pair it with a required Compose secret or another approved secret path so a
// misconfigured container does not start with an invented credential.
//
// Error / fatal format is deterministic so logs can identify the service,
// env-var name, rejection reason, and approved remediation without printing
// any credential value.

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

// ResolveCritical walks envVars in order and returns the first
// non-empty value, or a structured error if the chain is unset or the
// chosen value carries no recognized fleet-key prefix.
//
// slug is the service.yaml `id:` — it appears in the error so an
// operator or AI agent can copy-paste the exact remediation:
//
//	fleet-runner key provision <slug>
//	# issues a key that expires, writes it as FLEET_API_KEY into the
//	# service's .env on the dockerhost and restarts the service.
//
// Never mint a never-expiring key for this: every keystore key gets an
// expiry (fleet-state docs/architecture/ephemeral-credentials), and a service
// that should pick up a renewed key without a restart uses
// ResolveCriticalFile instead.
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
		return "", criticalErr(slug, strings.Join(envVars, ","), "unset", provisionFix(slug))
	}
	if !HasFleetPrefix(r.Key) {
		return "", criticalErr(slug, r.Source, unknownPrefixReason(), provisionFix(slug))
	}
	return r.Key, nil
}

// provisionFix is the remediation every critical-key error names: provision
// issues an expiring key (fleet-runner's default TTL), installs it where the
// service reads it and restarts the service. It used to say `key issue
// --never-expires`, which minted keys that never expire.
func provisionFix(slug string) string {
	return fmt.Sprintf("fleet-runner key provision %s (issues an expiring key, writes it as FLEET_API_KEY into the service's .env on the dockerhost and restarts it)", slug)
}

func unknownPrefixReason() string {
	return fmt.Sprintf("unknown_prefix (expected %s* or %s*)", KeyPrefixDynamic, KeyPrefixFallback)
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

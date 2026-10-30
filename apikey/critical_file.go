package apikey

import (
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/credfile"
)

// CriticalKey is a service principal key resolved by ResolveCriticalFile.
// When it comes from a file, it follows that file: a renewed key written over
// it is returned by Value within a minute, or at once through Rejected after
// a 401. When it comes from an environment variable it never changes, exactly
// like ResolveCritical's string. It never prints its value.
type CriticalKey struct {
	src    *credfile.Source
	static string
	from   string
}

// Value returns the current key. Call it for every request (it is cheap: a
// file is looked at no more than every 30 seconds) rather than keeping the
// string. Never log it.
func (k *CriticalKey) Value() string {
	if k.src != nil {
		return k.src.Value()
	}
	return k.static
}

// Rejected is the 401 path: it re-reads a file-backed key once and reports
// whether the file now holds a different key than used. Retry the request
// once when changed is true, never more. An env-var key never changes.
func (k *CriticalKey) Rejected(used string) (value string, changed bool) {
	if k.src != nil {
		return k.src.Rejected(used)
	}
	return k.static, k.static != used
}

// Source names the environment variable the key came from (the file
// variable, or one of the env-var chain). Safe to log.
func (k *CriticalKey) Source() string { return k.from }

// FileBacked reports whether the key follows a file.
func (k *CriticalKey) FileBacked() bool { return k.src != nil }

// Expiry is the key's expiry from the file's .meta sidecar, when known.
func (k *CriticalKey) Expiry() (time.Time, bool) {
	if k.src != nil {
		return k.src.Expiry()
	}
	return time.Time{}, false
}

// String names the key's source without its value.
func (k *CriticalKey) String() string { return "apikey.CriticalKey(" + k.from + ")" }

// GoString is String, for %#v.
func (k *CriticalKey) GoString() string { return k.String() }

// ResolveCriticalFile is ResolveCritical for a key that is delivered as a file
// and renewed in place. fileEnv names the variable holding the file's path
// (FLEET_API_KEY_FILE by convention); envVars is the ResolveCritical chain,
// used when fileEnv is unset:
//
//	key := apikey.MustResolveCriticalFile("go-fleet-dns-sync", "FLEET_API_KEY_FILE", "FLEET_API_KEY")
//	req.Header.Set(header.APIKey, key.Value())
//
// When fileEnv is set the file wins (as GRAPH_API_KEY_FILE does over
// GRAPH_API_KEY), so a renewed file is never shadowed by a stale env value.
// The file must be a regular file with no group or other permission bits,
// hold a key with a fleet prefix, and is re-read as package credfile
// describes; a refused or unreadable later version keeps the last good key.
// A service that sets only the env var gets exactly ResolveCritical's
// behaviour: the same checks, the same errors, a value that never changes.
func ResolveCriticalFile(slug, fileEnv string, envVars ...string) (*CriticalKey, error) {
	if slug == "" {
		return nil, errors.New("apikey.ResolveCriticalFile: empty slug (programmer error, not config)")
	}
	if fileEnv == "" {
		return nil, errors.New("apikey.ResolveCriticalFile: empty fileEnv (programmer error, not config)")
	}
	if path := strings.TrimSpace(os.Getenv(fileEnv)); path != "" {
		src, err := credfile.Open(criticalCredentialName(fileEnv), path, credfile.Options{
			Protected: true,
			Validate: func(v string) error {
				if !HasFleetPrefix(v) {
					return errors.New("unknown prefix")
				}
				return nil
			},
		})
		if err != nil {
			reason := "file_unreadable"
			var ce *credfile.Error
			if errors.As(err, &ce) {
				reason = "file_" + ce.Reason
				if ce.Reason == credfile.ReasonInvalid {
					reason = unknownPrefixReason()
				}
			}
			return nil, criticalErr(slug, fileEnv, reason, provisionFix(slug))
		}
		return &CriticalKey{src: src, from: fileEnv}, nil
	}
	if len(envVars) == 0 {
		return nil, criticalErr(slug, fileEnv, "unset", provisionFix(slug))
	}
	r := Resolve(envVars...)
	if !r.Found {
		return nil, criticalErr(slug, fileEnv+","+strings.Join(envVars, ","), "unset", provisionFix(slug))
	}
	if !HasFleetPrefix(r.Key) {
		return nil, criticalErr(slug, r.Source, unknownPrefixReason(), provisionFix(slug))
	}
	return &CriticalKey{static: r.Key, from: r.Source}, nil
}

// MustResolveCriticalFile is ResolveCriticalFile that exits the process on a
// configuration error, like MustResolveCritical.
func MustResolveCriticalFile(slug, fileEnv string, envVars ...string) *CriticalKey {
	k, err := ResolveCriticalFile(slug, fileEnv, envVars...)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return k
}

// criticalCredentialName is the metric name of the credential:
// FLEET_API_KEY_FILE -> fleet_api_key.
func criticalCredentialName(fileEnv string) string {
	name := strings.ToLower(strings.TrimSuffix(fileEnv, "_FILE"))
	if name == "" {
		name = "fleet_api_key"
	}
	return name
}

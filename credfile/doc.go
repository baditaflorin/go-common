// Package credfile reads a credential (an API key) from a file and keeps it
// current: a renewed key that is atomically written over the file is picked up
// without a restart, a callee's 401 triggers one immediate re-read, and a file
// that briefly goes missing leaves the last good value in use.
//
// It exists because keystore keys are getting an expiry (fleet-state
// docs/architecture/ephemeral-credentials, evidence-key-lifecycle.md section
// 5.3): a key that a service reads once at start dies with its expiry, while a
// key read through a Source follows the renewal controller's file.
//
// # The file contract
//
// The key file holds the key and optional trailing whitespace, at most
// MaxBytes (8 KiB by default). With Options.Protected it must be a regular file
// (not a symlink) with no group or other permission bits. Writers replace it
// atomically (temporary file in the same directory, then rename) and, for the
// replacement to be visible in a container, mount the directory rather than the
// single file.
//
// Next to it, "<path>.meta" may hold non-secret JSON about the key:
//
//	{"schema":1,"key_id":"...","expires_at":"2027-01-29T12:00:00Z",
//	 "issued_at":"...","renew_after":"...","successor_of":"...","key_sha256":"<hex>"}
//
// expires_at (RFC 3339 or unix seconds) becomes the expiry metric. key_sha256,
// when present, must be the SHA-256 of the trimmed key, or the meta is ignored
// as belonging to another key. A missing meta is not an error.
//
// # Reloading
//
// A Source checks the file (one stat, plus one for the meta) at most every
// CheckInterval (30 s by default, never more than 60 s), lazily, when the key is
// used or the metrics are scraped. It starts no goroutine. A changed inode,
// size or modification time makes it re-read the key and the meta. A read that
// fails keeps the last good value, counts the error, and counts every later use
// of that value as a stale use until a read succeeds again.
//
// Rejected is the 401 path: one immediate re-read (at most one per
// RejectInterval per file), and the caller retries once only if the value it
// gets back differs from the one that was rejected, so a bad key never loops.
//
// FLEET_CREDFILE_RELOAD=off (also false, 0, no, disabled) turns reloading off
// for the process: every Source reads its file once, at Open, as go-common did
// before this package existed.
//
// # Secrecy
//
// The value is returned by Value and Current and nowhere else: errors, String,
// GoString, Stats and the metrics carry the credential's name, its path and
// counters, never the value.
//
// # Metrics
//
// Snapshot returns the counters of every registered Source; promx exports them
// as fleet_credential_expiry_timestamp_seconds, fleet_credential_reload_total,
// fleet_credential_reload_errors_total and fleet_credential_stale_use_total,
// labelled by credential name.
package credfile

// Package fakekey provides an in-memory implementation of the
// apikey.Verifier interface for use in tests. It avoids the need to
// spin up a real keystore and eliminates HTTP round-trips from tests.
//
// Usage:
//
//	fk := fakekey.New()
//	fk.Allow("my-token", "my-service", "read")
//	fk.DenyNext() // next Verify call returns ErrInvalidKey
//
//	// use as apikey.Verifier anywhere:
//	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{
//	    Verifier: fk,
//	})
package fakekey

import (
	"context"
	"sync"

	"github.com/baditaflorin/go-common/apikey"
)

// Fake is an in-memory apikey.Verifier with configurable outcomes.
// All methods are safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	entries  map[string]*apikey.VerifyResult
	denyNext bool
	unavail  bool
	calls    []string // record of keys passed to Verify
}

// New returns an empty Fake. By default all keys are denied.
func New() *Fake {
	return &Fake{entries: make(map[string]*apikey.VerifyResult)}
}

// Allow registers key as a valid key that resolves to the given user
// and scope. Subsequent Verify calls for this key return the result
// immediately (no HTTP call).
func (f *Fake) Allow(key, user, scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[key] = &apikey.VerifyResult{User: user, Scope: scope}
}

// Deny removes key from the allowed set. Subsequent Verify calls for
// this key return ErrInvalidKey.
func (f *Fake) Deny(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

// DenyNext makes the next Verify call return ErrInvalidKey regardless
// of the key. After one call the Fake reverts to normal behaviour.
func (f *Fake) DenyNext() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denyNext = true
}

// SetUnavailable makes all subsequent Verify calls return
// ErrKeystoreUnavailable (simulating a keystore outage).
func (f *Fake) SetUnavailable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unavail = v
}

// Calls returns a snapshot of keys that were passed to Verify, in call
// order. Use in assertions: len(fk.Calls()) == 2.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// Reset clears all state (allowed keys, call log, pending deny/unavail).
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = make(map[string]*apikey.VerifyResult)
	f.denyNext = false
	f.unavail = false
	f.calls = nil
}

// Verify implements apikey.Verifier.
func (f *Fake) Verify(_ context.Context, key string) (*apikey.VerifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)

	if f.unavail {
		return nil, apikey.ErrKeystoreUnavailable
	}
	if f.denyNext {
		f.denyNext = false
		return nil, apikey.ErrInvalidKey
	}
	result, ok := f.entries[key]
	if !ok {
		return nil, apikey.ErrInvalidKey
	}
	return result, nil
}

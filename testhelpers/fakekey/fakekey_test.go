package fakekey_test

import (
	"context"
	"errors"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/testhelpers/fakekey"
)

func TestAllowedKeyVerifiesAndIsRecorded(t *testing.T) {
	f := fakekey.New()
	f.Allow("k1", "alice", "read")
	res, err := f.Verify(context.Background(), "k1")
	if err != nil || res.User != "alice" || res.Scope != "read" {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
	if got := f.Calls(); len(got) != 1 || got[0] != "k1" {
		t.Fatalf("Calls() = %v, want [k1]", got)
	}
}

func TestUnknownAndDeniedKeysAreInvalid(t *testing.T) {
	f := fakekey.New()
	if _, err := f.Verify(context.Background(), "nope"); !errors.Is(err, apikey.ErrInvalidKey) {
		t.Fatalf("unknown key: %v, want ErrInvalidKey", err)
	}
	f.Allow("k", "u", "s")
	f.Deny("k")
	if _, err := f.Verify(context.Background(), "k"); !errors.Is(err, apikey.ErrInvalidKey) {
		t.Fatalf("denied key: %v, want ErrInvalidKey", err)
	}
}

func TestDenyNextRejectsExactlyOneCall(t *testing.T) {
	f := fakekey.New()
	f.Allow("k", "u", "s")
	f.DenyNext()
	if _, err := f.Verify(context.Background(), "k"); !errors.Is(err, apikey.ErrInvalidKey) {
		t.Fatalf("first call: %v, want ErrInvalidKey", err)
	}
	if _, err := f.Verify(context.Background(), "k"); err != nil {
		t.Fatalf("second call: %v, want success", err)
	}
}

func TestUnavailableAndReset(t *testing.T) {
	f := fakekey.New()
	f.Allow("k", "u", "s")
	f.SetUnavailable(true)
	if _, err := f.Verify(context.Background(), "k"); !errors.Is(err, apikey.ErrKeystoreUnavailable) {
		t.Fatalf("unavailable: %v, want ErrKeystoreUnavailable", err)
	}
	f.Reset()
	if _, err := f.Verify(context.Background(), "k"); !errors.Is(err, apikey.ErrInvalidKey) {
		t.Fatalf("after Reset the key should be gone and the keystore back: %v", err)
	}
	if got := f.Calls(); len(got) != 1 {
		t.Fatalf("Reset should clear the call log; Calls() = %v", got)
	}
}

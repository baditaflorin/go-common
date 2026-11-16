package proxybudget

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPoolConfigStrictIdentityLimitsAndDestinations(t *testing.T) {
	base := PoolConfig{ProviderID: "provider-a", PoolID: "residential-a", AllowedHosts: []string{"Example.COM."}, Config: configFor(t)}
	file := filepath.Join(t.TempDir(), "budgets.json")
	write := func(pools []PoolConfig) {
		t.Helper()
		b, err := json.Marshal(map[string]any{"pools": pools})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write([]PoolConfig{base})
	got, err := LoadPoolConfigs(file)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if !got[0].AllowsHost("example.com") || !got[0].AllowsHost("EXAMPLE.COM.") || got[0].AllowsHost("evil.example.com") || got[0].AllowsHost("") {
		t.Fatal("host matching widened policy")
	}
	for name, change := range map[string]func(*PoolConfig){
		"provider": func(c *PoolConfig) { c.ProviderID = "../../bad" }, "pool": func(c *PoolConfig) { c.PoolID = "" },
		"empty_hosts": func(c *PoolConfig) { c.AllowedHosts = nil }, "wildcard": func(c *PoolConfig) { c.AllowedHosts = []string{"*.example.com"} },
		"url": func(c *PoolConfig) { c.AllowedHosts = []string{"https://example.com"} }, "empty_host": func(c *PoolConfig) { c.AllowedHosts = []string{""} },
		"double_dot": func(c *PoolConfig) { c.AllowedHosts = []string{"example..com"} }, "cap": func(c *PoolConfig) { c.MaxRequestBytes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			change(&p)
			write([]PoolConfig{p})
			if _, err := LoadPoolConfigs(file); !errors.Is(err, ErrConfig) {
				t.Fatal(err)
			}
		})
	}
	write([]PoolConfig{base, base})
	if _, err = LoadPoolConfigs(file); !errors.Is(err, ErrConfig) {
		t.Fatal("duplicate allowed", err)
	}
	other := base
	other.PoolID = "other"
	write([]PoolConfig{base, other})
	if _, err = LoadPoolConfigs(file); !errors.Is(err, ErrConfig) {
		t.Fatal("shared ledger dir allowed", err)
	}
	for _, bad := range []string{`{}`, `{"pools":[],"unknown":true}`, `{"pools":[]}{}`, `not json`} {
		_ = os.WriteFile(file, []byte(bad), 0600)
		if _, err = LoadPoolConfigs(file); !errors.Is(err, ErrConfig) {
			t.Fatal(bad, err)
		}
	}
	if _, err = LoadPoolConfigs(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	if _, err = LoadPoolConfigs(t.TempDir()); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	_ = os.WriteFile(file, make([]byte, (1<<20)+1), 0600)
	if _, err = LoadPoolConfigs(file); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
}

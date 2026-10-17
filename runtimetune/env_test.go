package runtimetune

import (
	"testing"

	"github.com/baditaflorin/go-common/env"
)

// Each Check must accept exactly the values the reader accepts: if the reader's
// rule changes and the declaration does not, the start-up validation would warn
// about good values or stay quiet about bad ones.
func TestDeclaredChecksAgreeWithTheReaders(t *testing.T) {
	type tc struct {
		v        *env.Var
		value    string
		accepted func(Config) bool // did the reader take the value?
	}
	def := DefaultConfig()
	cases := []tc{
		{envMemLimitRatio, "0.5", func(c Config) bool { return c.MemLimitRatio == 0.5 }},
		{envMemLimitRatio, "0", func(c Config) bool { return c.MemLimitRatio == 0 }},
		{envMemLimitRatio, "1", func(c Config) bool { return c.MemLimitRatio == 1 }},
		{envMemLimitRatio, "abc", func(c Config) bool { return c.MemLimitRatio != def.MemLimitRatio }},
		{envMinHeapBytes, "1048576", func(c Config) bool { return c.MinHeapTarget == 1048576 }},
		{envMinHeapBytes, "0", func(c Config) bool { return c.MinHeapTarget == 0 }},
		{envMinHeapBytes, "-1", func(c Config) bool { return c.MinHeapTarget == -1 }},
		{envBaseGOGC, "150", func(c Config) bool { return c.BaseGOGC == 150 }},
		{envBaseGOGC, "0", func(c Config) bool { return c.BaseGOGC == 0 }},
		{envBaseGOGC, "x", func(c Config) bool { return false }},
		{envMaxGOGC, "300", func(c Config) bool { return c.MaxGOGC == 300 }},
		{envMaxGOGC, "-5", func(c Config) bool { return c.MaxGOGC == -5 }},
		{envInterval, "7s", func(c Config) bool { return c.Interval.String() == "7s" }},
		{envInterval, "0s", func(c Config) bool { return c.Interval == 0 }},
		{envInterval, "soon", func(c Config) bool { return c.Interval != def.Interval }},
	}
	for _, c := range cases {
		t.Setenv(c.v.Name, c.value)
		read := c.accepted(ConfigFromEnv())
		checked := c.v.Validate(c.value)
		if read != (checked == nil) {
			t.Errorf("%s=%q: the reader accepted it = %v, but the declared Check says %v", c.v.Name, c.value, read, checked)
		}
		t.Setenv(c.v.Name, "")
	}
}

func TestKillSwitchCheckMatchesTheReader(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", "on", "enabled", "0", "false", "off", "Disabled", "ture", "maybe", " on "} {
		reader := killSwitchRecognised(v)
		if got := checkKillSwitch(v) == nil; got != reader {
			t.Errorf("%q: Check accepts = %v, reader recognises = %v", v, got, reader)
		}
	}
}

// killSwitchRecognised is whether killSwitchEnabled treated v as a real answer
// (as opposed to the "unrecognised, so disable" fallback), without its log line.
func killSwitchRecognised(v string) bool {
	switch v {
	case "":
		return true
	}
	switch trimLower(v) {
	case "1", "true", "yes", "on", "enabled", "0", "false", "no", "off", "disabled":
		return true
	}
	return false
}

func trimLower(s string) string {
	out := []byte{}
	for _, b := range []byte(s) {
		if b == ' ' || b == '\t' {
			continue
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		out = append(out, b)
	}
	return string(out)
}

func TestVariablesAreRegistered(t *testing.T) {
	seen := map[string]bool{}
	for _, i := range env.Default.Snapshot(nil) {
		seen[i.Name] = true
		if i.Name == EnvKillSwitch && i.Owner != "runtimetune" {
			t.Errorf("owner of %s = %q, want runtimetune", i.Name, i.Owner)
		}
	}
	for _, n := range []string{EnvKillSwitch, EnvMemLimitRatio, EnvMinHeapBytes, EnvBaseGOGC, EnvMaxGOGC, EnvInterval} {
		if !seen[n] {
			t.Errorf("%s is not in the registry", n)
		}
	}
}

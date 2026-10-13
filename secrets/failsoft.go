package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Fail-soft configuration. A service must be able to START while the primary vault host is down:
//
//	FLEET_SECRETS_FALLBACK_URLS  comma-separated extra vault base URLs, tried in order after the primary
//	                             (a follower on another site). Each must be https on an approved vault host.
//	FLEET_SECRETS_CACHE_DIR      directory for the encrypted last-known-good cache (opt-in; must survive restarts)
//	FLEET_SECRETS_CACHE_MAX_AGE  Go duration, default 168h; older cached values are ignored
//
// The cache holds each secret AES-256-GCM encrypted under a key derived from the caller's own fleet API key, so the files are
// useless to anyone without that key, and 0600 inside a 0700 directory. It is written after every successful read and used only when
// every URL failed with an outage-type error (never for 401/403/404).

const (
	envFallbackURLs  = "FLEET_SECRETS_FALLBACK_URLS"
	envCacheDir      = "FLEET_SECRETS_CACHE_DIR"
	envCacheMaxAge   = "FLEET_SECRETS_CACHE_MAX_AGE"
	defaultCacheAge  = 168 * time.Hour
	cacheFileVersion = byte(1)
	cacheRewriteGap  = time.Hour
)

var approvedVaultHost = regexp.MustCompile(`^fleet-secrets[a-z0-9-]*\.(0exec|0mcp|0own|0docker)\.com$`)

// ApprovedVaultURL reports whether raw is an acceptable vault base URL: https, an approved fleet-secrets host, no credentials,
// path, query or non-443 port. The API key is sent to every URL in the list, so the list is an allowlist, not free text.
func ApprovedVaultURL(raw string) bool {
	p, err := url.Parse(raw)
	if err != nil || p.Scheme != "https" || p.User != nil || (p.Path != "" && p.Path != "/") || p.RawQuery != "" || p.Fragment != "" {
		return false
	}
	return (p.Port() == "" || p.Port() == "443") && approvedVaultHost.MatchString(strings.ToLower(p.Hostname()))
}

// WithFallbacks appends vault base URLs tried, in order, when the primary has an outage-type failure. It does no host
// validation (programmatic callers own that; see ApprovedVaultURL). Returns c for chaining.
func (c *Client) WithFallbacks(urls ...string) *Client {
	for _, u := range urls {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" && u != c.baseURL {
			c.fallbacks = append(c.fallbacks, u)
		}
	}
	return c
}

// WithCache enables the encrypted last-known-good cache in dir. maxAge <= 0 means defaultCacheAge.
func (c *Client) WithCache(dir string, maxAge time.Duration) *Client {
	if strings.TrimSpace(dir) == "" {
		return c
	}
	if maxAge <= 0 {
		maxAge = defaultCacheAge
	}
	c.cache = &diskCache{dir: dir, maxAge: maxAge}
	return c
}

// WithEnv applies FLEET_SECRETS_FALLBACK_URLS / FLEET_SECRETS_CACHE_DIR / FLEET_SECRETS_CACHE_MAX_AGE. Unapproved fallback URLs are
// dropped silently (the key must not travel to them); with none of the variables set it is a no-op, so it is safe to call everywhere.
func (c *Client) WithEnv(getenv func(string) string) *Client {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, u := range strings.Split(getenv(envFallbackURLs), ",") {
		if u = strings.TrimSpace(u); ApprovedVaultURL(u) {
			c.WithFallbacks(u)
		}
	}
	age, _ := time.ParseDuration(strings.TrimSpace(getenv(envCacheMaxAge)))
	return c.WithCache(getenv(envCacheDir), age)
}

type diskCache struct {
	dir    string
	maxAge time.Duration
}

type cacheRecord struct {
	Value    string    `json:"v"`
	StoredAt time.Time `json:"t"`
}

func cacheKey(apiKey string) []byte {
	k := sha256.Sum256([]byte("go-common/secrets/cache/v1\x00" + apiKey))
	return k[:]
}

func (d *diskCache) path(name string) string { return filepath.Join(d.dir, name+".enc") }

func (d *diskCache) open(apiKey string) (cipher.AEAD, error) {
	if apiKey == "" {
		return nil, errors.New("no key") // never cache under an empty key
	}
	block, err := aes.NewCipher(cacheKey(apiKey))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// store is best effort: a cache that cannot be written must never fail a successful read.
func (d *diskCache) store(name, value, apiKey string) {
	if d == nil {
		return
	}
	if v, age, ok := d.load(name, apiKey); ok && v == value && age < cacheRewriteGap {
		return
	}
	aead, err := d.open(apiKey)
	if err != nil {
		return
	}
	plain, _ := json.Marshal(cacheRecord{Value: value, StoredAt: time.Now().UTC()})
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	blob := append([]byte{cacheFileVersion}, nonce...)
	blob = aead.Seal(blob, nonce, plain, []byte(name))
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(d.dir, ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(blob); err != nil || tmp.Chmod(0o600) != nil || tmp.Close() != nil {
		return
	}
	_ = os.Rename(tmp.Name(), d.path(name))
}

func (d *diskCache) load(name, apiKey string) (string, time.Duration, bool) {
	if d == nil {
		return "", 0, false
	}
	aead, err := d.open(apiKey)
	if err != nil {
		return "", 0, false
	}
	blob, err := os.ReadFile(d.path(name))
	if err != nil || len(blob) < 1+aead.NonceSize()+aead.Overhead() || blob[0] != cacheFileVersion {
		return "", 0, false
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, blob[1:1+n], blob[1+n:], []byte(name))
	if err != nil {
		return "", 0, false
	}
	var rec cacheRecord
	if json.Unmarshal(plain, &rec) != nil || rec.Value == "" {
		return "", 0, false
	}
	age := time.Since(rec.StoredAt)
	if age < 0 {
		age = 0
	}
	if age > d.maxAge {
		return "", 0, false
	}
	return rec.Value, age, true
}

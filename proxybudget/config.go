package proxybudget

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
)

// PoolConfig binds one provider/pool allocation to its byte budget and allowed
// destination hosts. It contains no proxy URL or credentials. Callers enforce
// AllowsHost on every target, including redirects and browser subresources,
// in addition to their normal SSRF checks.
type PoolConfig struct {
	ProviderID   string   `json:"provider_id"`
	PoolID       string   `json:"pool_id"`
	AllowedHosts []string `json:"allowed_hosts"`
	Config
}

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var dnsHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// AllowsHost performs an exact normalized host match, never wildcard expansion.
func (c PoolConfig) AllowsHost(host string) bool {
	host = normalizeHost(host)
	for _, allowed := range c.AllowedHosts {
		if host != "" && host == normalizeHost(allowed) {
			return true
		}
	}
	return false
}

// LoadPoolConfigs reads the strict {"pools":[...]} config without opening any
// ledgers. Duplicate pool identities or state directories, unknown fields,
// missing/invalid limits, empty host policies and oversized files fail closed.
func LoadPoolConfigs(path string) ([]PoolConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrConfig
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, ErrConfig
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	d.DisallowUnknownFields()
	var config struct {
		Pools []PoolConfig `json:"pools"`
	}
	if err = d.Decode(&config); err != nil {
		return nil, ErrConfig
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, ErrConfig
	}
	if len(config.Pools) == 0 || len(config.Pools) > 32 {
		return nil, ErrConfig
	}
	ids, dirs := map[string]bool{}, map[string]bool{}
	for _, p := range config.Pools {
		key := p.ProviderID + "/" + p.PoolID
		if !safeID.MatchString(p.ProviderID) || !safeID.MatchString(p.PoolID) || ids[key] || dirs[p.StateDir] || p.validate() != nil || len(p.AllowedHosts) == 0 || len(p.AllowedHosts) > 256 {
			return nil, ErrConfig
		}
		ids[key] = true
		dirs[p.StateDir] = true
		for _, h := range p.AllowedHosts {
			h = normalizeHost(h)
			if h == "" || strings.Contains(h, "..") || (net.ParseIP(h) == nil && !dnsHost.MatchString(h)) {
				return nil, ErrConfig
			}
		}
	}
	return config.Pools, nil
}

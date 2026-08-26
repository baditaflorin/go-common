package proxysupplier

// New reads PROXY_SUPPLIER (and related vars) from the environment and returns
// the matching Supplier. It is a convenience wrapper for EnvConfig + NewFromConfig.
func New() Supplier {
	return NewFromConfig(EnvConfig())
}

// NewFromConfig selects the supplier described by cfg.
// The self-proxy guard is always applied.
func NewFromConfig(cfg Config) Supplier {
	rules := parseNoProxy(cfg.NoProxy)

	var s Supplier
	switch cfg.Supplier {
	case "plain_proxies":
		rawURL := buildURL("http", cfg.Host, cfg.Port, cfg.Username, cfg.Password)
		if rawURL == "" {
			return noneSupplier{}
		}
		s = &urlSupplier{name: "plain_proxies", rawURL: rawURL, rules: rules, keepAlive: cfg.KeepAliveEnabled}

	case "env":
		rawURL := cfg.ExternalProxyURL
		if rawURL == "" {
			proto := cfg.Protocol
			if proto == "" {
				proto = "http"
			}
			rawURL = buildURL(proto, cfg.Host, cfg.Port, cfg.Username, cfg.Password)
		}
		if rawURL == "" {
			return noneSupplier{}
		}
		s = &urlSupplier{name: "env", rawURL: rawURL, rules: rules, keepAlive: cfg.KeepAliveEnabled}

	case "multi":
		ms := newMultiSupplier(cfg.ProxyURLs, cfg.ProxyWeights)
		if ms == nil {
			return noneSupplier{}
		}
		ms.rules = rules
		ms.keepAlive = cfg.KeepAliveEnabled
		return ms // self-proxy guard already applied inside newMultiSupplier

	default:
		return noneSupplier{}
	}

	if isSelfProxy(s.ProxyURL()) {
		return noneSupplier{}
	}
	return s
}

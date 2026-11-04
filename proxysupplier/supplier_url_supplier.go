package proxysupplier

type urlSupplier struct {
	name      string
	rawURL    string
	rules     *noProxyRules
	keepAlive bool
}

func (s *urlSupplier) Name() string { return s.name }

func (s *urlSupplier) ProxyURL() string { return s.rawURL }

func (s *urlSupplier) Bypass(host string) bool {
	return s.rules.Match(host)
}

func (s *urlSupplier) KeepAliveEnabled() bool { return s.keepAlive }

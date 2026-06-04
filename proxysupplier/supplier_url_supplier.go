package proxysupplier

type urlSupplier struct {
	name   string
	rawURL string
	rules  *noProxyRules
}

func (s *urlSupplier) Name() string { return s.name }

func (s *urlSupplier) ProxyURL() string { return s.rawURL }

func (s *urlSupplier) Bypass(host string) bool {
	if s.rules == nil {
		return false
	}
	return s.rules.Match(host)
}

package proxysupplier

import (
	"math/rand"
)

// multiSupplier picks a proxy URL for each request using weighted random
// selection. It satisfies [Supplier].
type multiSupplier struct {
	entries []multiEntry
	total   int
	rules   *noProxyRules
}

func (m *multiSupplier) Bypass(host string) bool {
	if m.rules == nil {
		return false
	}
	return m.rules.Match(host)
}

func (m *multiSupplier) Name() string { return "multi" }

// ProxyURL returns a weighted-random proxy URL from the pool.
// Called per-request by HTTPClient's Proxy function.
func (m *multiSupplier) ProxyURL() string {
	if len(m.entries) == 1 {
		return m.entries[0].rawURL
	}
	//nolint:gosec // non-crypto random is fine for proxy selection
	r := rand.Intn(m.total)
	for _, e := range m.entries {
		r -= e.weight
		if r < 0 {
			return e.rawURL
		}
	}
	return m.entries[len(m.entries)-1].rawURL
}

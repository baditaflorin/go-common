package client

import (
	"net/url"
	"strings"
)

// RedirectParam is one observed query parameter on a real network
// request whose name pattern matches a known open-redirect sink.
type RedirectParam struct {
	OnURL string `json:"on_url"` // request URL where the param appeared
	Key   string `json:"key"`    // the parameter name (e.g. "next")
	Value string `json:"value"`  // the observed value (may itself be a URL)
}

// RedirectParams scans every recorded network URL (not just the
// rendered page) and returns each query parameter whose key matches a
// known redirect sink. Use this as the seed list for fuzzing instead
// of guessing parameter names from corpora.
func RedirectParams(r *FetchResult) []RedirectParam {
	if r == nil {
		return nil
	}
	keys := make(map[string]struct{}, len(redirectParamKeys))
	for _, k := range redirectParamKeys {
		keys[k] = struct{}{}
	}
	out := []RedirectParam{}
	seen := map[string]struct{}{}
	for _, e := range r.Network {
		u, err := url.Parse(e.URL)
		if err != nil || u.RawQuery == "" {
			continue
		}
		for k, vs := range u.Query() {
			if _, hit := keys[strings.ToLower(k)]; !hit {
				continue
			}
			for _, v := range vs {
				dedupe := e.URL + "?" + k + "=" + v
				if _, dup := seen[dedupe]; dup {
					continue
				}
				seen[dedupe] = struct{}{}
				out = append(out, RedirectParam{OnURL: e.URL, Key: k, Value: v})
			}
		}
	}
	return out
}

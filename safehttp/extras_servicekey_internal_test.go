package safehttp

import (
	"net/http"
	"testing"
)

// TestSetServiceKey pins the rule for the key on a trace or backoff post: the
// service's own FLEET_API_KEY goes to a fleet host and nowhere else, a key
// already on the request is kept, and an unset or placeholder variable sends
// nothing.
func TestSetServiceKey(t *testing.T) {
	const key = "ak_test_servicekey_not_a_real_key"

	cases := []struct {
		name    string
		env     string
		url     string
		preset  string
		wantKey string
	}{
		{name: "public fleet name", env: key, url: "https://fleet-call-tracer.0exec.com/traces", wantKey: key},
		{name: "fleet apex itself", env: key, url: "https://0exec.com/traces", wantKey: key},
		{name: "bare container name", env: key, url: "http://go-fleet-call-tracer:8080/traces", wantKey: key},
		{name: "private address", env: key, url: "http://10.70.10.30:9000/backoff", wantKey: key},
		{name: "loopback", env: key, url: "http://127.0.0.1:9000/backoff", wantKey: key},
		{name: "key is trimmed", env: "  " + key + "\n", url: "https://fleet-backoff-coordinator.0exec.com/backoff", wantKey: key},

		{name: "third party host", env: key, url: "https://collector.example.com/traces"},
		{name: "fleet name as a prefix of a foreign domain", env: key, url: "https://0exec.com.evil.example/traces"},
		{name: "fleet name glued to a foreign label", env: key, url: "https://evil0exec.com/traces"},
		{name: "public address", env: key, url: "https://203.0.113.9/traces"},

		{name: "unset", env: "", url: "https://fleet-call-tracer.0exec.com/traces"},
		{name: "blank", env: "  ", url: "https://fleet-call-tracer.0exec.com/traces"},
		{name: "placeholder", env: "default_token", url: "https://fleet-call-tracer.0exec.com/traces"},

		{name: "an existing key is kept", env: key, url: "https://fleet-call-tracer.0exec.com/traces", preset: "ak_caller_supplied", wantKey: "ak_caller_supplied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_API_KEY", tc.env)
			t.Setenv("FLEET_DIRECT_DOMAINS", "")

			req, err := http.NewRequest(http.MethodPost, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.preset != "" {
				req.Header.Set("X-API-Key", tc.preset)
			}
			setServiceKey(req)
			if got := req.Header.Get("X-API-Key"); got != tc.wantKey {
				t.Errorf("X-API-Key = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

// TestSetServiceKey_NilSafe: the helper runs on the trace and backoff paths,
// which must never panic into the calling service.
func TestSetServiceKey_NilSafe(t *testing.T) {
	t.Setenv("FLEET_API_KEY", "ak_test_servicekey_not_a_real_key")
	setServiceKey(nil)
	setServiceKey(&http.Request{Header: http.Header{}})
}

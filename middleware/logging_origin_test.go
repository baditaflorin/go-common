package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
)

func logLine(t *testing.T, remote string, hdr map[string]string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	saved := defaultLogger
	defaultLogger = captureLogger(&buf, slog.LevelInfo)
	defer func() { defaultLogger = saved }()
	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &m); err != nil {
		t.Fatalf("log line: %v (%q)", err, buf.String())
	}
	return m
}

func TestTheAccessLogNamesTheClientBehindTheGatewayAndTheNode(t *testing.T) {
	m := logLine(t, "10.10.10.10:52340", map[string]string{"X-Real-IP": "203.0.113.7", "X-Fleet-Node": "infer-runtime"})
	if m["client_ip"] != "203.0.113.7" {
		t.Errorf("client_ip = %v, want 203.0.113.7", m["client_ip"])
	}
	if m["node"] != "infer-runtime" {
		t.Errorf("node = %v, want infer-runtime", m["node"])
	}
	if m["ip"] != "10.10.10.10:52340" {
		t.Errorf("ip must stay the connection's peer, got %v", m["ip"])
	}
}

func TestAClientThatReachesTheServiceDirectlyCannotChooseTheLoggedAddress(t *testing.T) {
	m := logLine(t, "198.51.100.9:4444", map[string]string{"X-Real-IP": "203.0.113.7"})
	if _, ok := m["client_ip"]; ok {
		t.Errorf("a public peer's X-Real-IP was believed: %v", m["client_ip"])
	}
}

func TestMalformedOriginHeadersAreNotLogged(t *testing.T) {
	m := logLine(t, "10.10.10.10:1", map[string]string{"X-Real-IP": "not-an-ip", "X-Fleet-Node": "Evil Node\"\n"})
	for _, k := range []string{"client_ip", "node"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s = %v, want it absent", k, v)
		}
	}
}

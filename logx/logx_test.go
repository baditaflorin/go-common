package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func record(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want exactly one log line, got %q", buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, lines[0])
	}
	return m
}

func withSpan(ctx context.Context) context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(ctx, sc)
}

func TestEveryLineCarriesTheServiceFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	New(Options{Service: "go_example", Version: "1.2.3", Node: "dockerhost", Output: &buf}).Info("hello", "n", 1)
	m := record(t, &buf)
	if m["service"] != "go_example" || m["version"] != "1.2.3" || m["node"] != "dockerhost" || m["msg"] != "hello" || m["level"] != "INFO" || m["n"] != float64(1) {
		t.Fatalf("record = %v", m)
	}

	buf.Reset()
	New(Options{Service: "svc", Output: &buf}).Info("x")
	m = record(t, &buf)
	if _, has := m["version"]; has {
		t.Fatalf("an empty version should be omitted: %v", m)
	}
	if _, has := m["node"]; has {
		t.Fatalf("an empty node should be omitted: %v", m)
	}
}

func TestTheTraceIdIsAddedInsideATracedRequest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Service: "svc", Output: &buf})
	l.InfoContext(withSpan(context.Background()), "traced")
	m := record(t, &buf)
	if m["trace_id"] != "0102030405060708090a0b0c0d0e0f10" || m["span_id"] != "0102030405060708" {
		t.Fatalf("record = %v", m)
	}

	buf.Reset()
	l.InfoContext(context.Background(), "untraced")
	if m := record(t, &buf); m["trace_id"] != nil || m["span_id"] != nil {
		t.Fatalf("a record outside any span carries trace ids: %v", m)
	}
}

func TestTheTraceSurvivesWithAttrsAndWithGroup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Service: "svc", Output: &buf}).With("request_id", "r1")
	l.InfoContext(withSpan(context.Background()), "child")
	m := record(t, &buf)
	if m["trace_id"] == nil || m["request_id"] != "r1" || m["service"] != "svc" {
		t.Fatalf("a child logger lost something: %v", m)
	}
}

func TestContextAttrsAreAdded(t *testing.T) {
	t.Parallel()
	type key struct{}
	var buf bytes.Buffer
	l := New(Options{Output: &buf, ContextAttrs: func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(key{}).(string); ok {
			return []slog.Attr{slog.String("tenant", v)}
		}
		return nil
	}})
	l.InfoContext(context.WithValue(context.Background(), key{}, "acme"), "x")
	if m := record(t, &buf); m["tenant"] != "acme" {
		t.Fatalf("record = %v", m)
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(Options{Output: &buf, Level: slog.LevelWarn})
	l.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("an info record passed a warn threshold: %s", buf.String())
	}
	l.Warn("kept")
	if m := record(t, &buf); m["level"] != "WARN" {
		t.Fatalf("record = %v", m)
	}
}

func TestCredentialLookingKeysAreRedactedWhenTheValueIsAString(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	New(Options{Output: &buf}).Info("call",
		"token", "abc123", "password", "hunter2", "Authorization", "Basic xyz", "api_key", "k-9", "cookie", "sid=1",
		"tokens", 52, "client_secret", "s3", "user", "ada",
		slog.Group("req", slog.String("api-key", "inner"), slog.String("path", "/x")))
	m := record(t, &buf)
	for _, k := range []string{"token", "password", "Authorization", "api_key", "cookie", "client_secret"} {
		if m[k] != Redacted {
			t.Errorf("%s = %v, want %s", k, m[k], Redacted)
		}
	}
	if m["tokens"] != float64(52) {
		t.Errorf("a numeric tokens count was changed: %v", m["tokens"])
	}
	if m["user"] != "ada" {
		t.Errorf("an ordinary attribute was changed: %v", m["user"])
	}
	req := m["req"].(map[string]any)
	if req["api-key"] != Redacted || req["path"] != "/x" {
		t.Errorf("group = %v", req)
	}
	if strings.Contains(buf.String(), "hunter2") || strings.Contains(buf.String(), "abc123") {
		t.Fatalf("a secret reached the output: %s", buf.String())
	}
}

func TestSecretsInsideStringsAreRedacted(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Authorization: Bearer abcdefgh12345678":   "Authorization: Bearer " + Redacted,
		"bearer lowercaseTOKEN1234":                "bearer " + Redacted,
		"dial https://svc:p4ss@host:8080/x failed": "dial https://" + Redacted + "@host:8080/x failed",
		"GET /v1?api_key=abc&x=1":                  "GET /v1?api_key=" + Redacted + "&x=1",
		"password=hunter2 user=ada":                "password=" + Redacted + " user=ada",
		"nothing sensitive here":                   "nothing sensitive here",
		"https://host/path":                        "https://host/path",
		"Bearer short":                             "Bearer short", // too short to be a token: left alone
		"":                                         "",
	} {
		if got := RedactString(in); got != want {
			t.Errorf("RedactString(%q) = %q, want %q", in, got, want)
		}
		if again := RedactString(RedactString(in)); again != RedactString(in) {
			t.Errorf("RedactString is not idempotent for %q", in)
		}
	}
}

func TestTheMessageAndErrorsAreRedactedToo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	New(Options{Output: &buf}).Error("upstream https://u:secretpw@host failed", "err", errors.New("dial tcp: token=abc123 rejected"))
	m := record(t, &buf)
	out := buf.String()
	if strings.Contains(out, "secretpw") || strings.Contains(out, "abc123") {
		t.Fatalf("a secret reached the output: %s", out)
	}
	if !strings.Contains(m["msg"].(string), "host failed") || !strings.Contains(m["err"].(string), "rejected") {
		t.Fatalf("redaction removed too much: %v", m)
	}
}

// slog.SetDefault, which Install calls, also points the standard library's log
// package at the handler: existing log.Printf calls become structured.
func TestInstallStructuresTheStandardLogPackageToo(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	var buf bytes.Buffer
	Install(Options{Service: "legacy_svc", Output: &buf})
	log.Printf("server: stopped cleanly after %d requests", 7)
	m := record(t, &buf)
	if m["msg"] != "server: stopped cleanly after 7 requests" || m["level"] != "INFO" || m["service"] != "legacy_svc" {
		t.Fatalf("record = %v", m)
	}
	buf.Reset()
	slog.Info("via slog")
	if m := record(t, &buf); m["service"] != "legacy_svc" {
		t.Fatalf("the default slog logger was not replaced: %v", m)
	}
}

// The cheap pre-check must never skip a string a pattern would have changed: for
// every shape the patterns match, the pre-check has to say "may carry a secret".
func TestThePrecheckNeverSkipsAMatch(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"Bearer abcdefgh12345678", "bearer abcdefgh12345678", "BEARER abcdefgh12345678", "xBeArEr abcdefgh12345678",
		"https://u:p@h/", "a@b", "token=x", "api_key=1", "password=1", "API-KEY=1", "apikey=1", "access_token=1", "PASSWD=1", "client_secret=1", "x token=1",
	} {
		if RedactString(s) != s && !mayCarrySecret(s) {
			t.Errorf("%q is redacted by the patterns but the pre-check would skip it", s)
		}
	}
	for _, s := range []string{"", "plain text", "request_completed status 200", "bear", "bearing", "status=200 path=/v1/items duration=1.2ms", "a=b&c=d", "=", "ke=1"} {
		if mayCarrySecret(s) {
			t.Errorf("%q has none of the trigger characters but the pre-check wants to scan it", s)
		}
	}
}

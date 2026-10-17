package server

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/middleware"
)

// restoreLogging puts back everything installLogs changes process-wide.
func restoreLogging(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
		middleware.SetLogger(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	})
}

func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestLogsAreLeftAloneByDefault(t *testing.T) {
	restoreLogging(t)
	t.Setenv("LOG_FORMAT", "")
	before := slog.Default()
	New(&config.Config{AppName: "go_logs_test", Version: "1", Port: "0"})
	if slog.Default() != before {
		t.Fatal("New changed the process logger without being asked to")
	}
}

func TestWithStructuredLogsInstallsLogx(t *testing.T) {
	restoreLogging(t)
	var buf bytes.Buffer
	srv := New(&config.Config{AppName: "go_logs_test", Version: "1.2.3", Port: "0"}, WithStructuredLogs(), withLogOutput(&buf))
	srv.Mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	log.Printf("legacy line %d", 3) // the standard log package
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))

	var legacy, access map[string]any
	for _, m := range jsonLines(t, &buf) {
		switch m["msg"] {
		case "legacy line 3":
			legacy = m
		case "request_completed":
			access = m
		}
	}
	if legacy == nil || legacy["service"] != "go_logs_test" || legacy["version"] != "1.2.3" {
		t.Fatalf("the standard log package was not structured: %v", legacy)
	}
	if access == nil || access["service"] != "go_logs_test" || access["path"] != "/ping" || access["request_id"] == nil {
		t.Fatalf("the access log is missing fields: %v", access)
	}
}

func TestLogFormatJSONInstallsLogxWithoutACodeChange(t *testing.T) {
	restoreLogging(t)
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("LOG_LEVEL", "warn")
	var buf bytes.Buffer
	New(&config.Config{AppName: "go_logs_env", Version: "9", Port: "0"}, withLogOutput(&buf))
	slog.Info("below the level")
	slog.Warn("at the level")
	lines := jsonLines(t, &buf)
	if len(lines) != 1 || lines[0]["msg"] != "at the level" || lines[0]["service"] != "go_logs_env" {
		t.Fatalf("lines = %v, want only the warn record, with the service", lines)
	}
}

func TestLogSettingsAreValidatedAtStartup(t *testing.T) {
	for environ, name := range map[string]string{"LOG_FORMAT=yaml": "LOG_FORMAT", "LOG_LEVEL=loud": "LOG_LEVEL"} {
		var found bool
		for _, p := range env.Default.Validate([]string{environ}) {
			found = found || (p.Name == name && p.Level == "error")
		}
		if !found {
			t.Errorf("%s was not reported", environ)
		}
	}
	for _, ok := range []string{"LOG_FORMAT=json", "LOG_FORMAT=Text", "LOG_LEVEL=DEBUG", "LOG_LEVEL=warning"} {
		for _, p := range env.Default.Validate([]string{ok}) {
			t.Errorf("%s was reported: %+v", ok, p)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "INFO": slog.LevelInfo, "debug": slog.LevelDebug, "Warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := parseLevel(in); !ok || got != want {
			t.Errorf("parseLevel(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := parseLevel("loud"); ok {
		t.Error("parseLevel accepted loud")
	}
}

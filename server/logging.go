package server

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/logx"
	"github.com/baditaflorin/go-common/middleware"
)

var (
	envLogFormat = env.Declare(env.Var{
		Name: "LOG_FORMAT", Kind: env.KindString, Default: "text",
		Doc: "Set to json to switch the process to structured JSON logs (package logx): the access log and everything written through the standard log package gain the service, version and node, and the trace id inside a traced request, and credentials are redacted. text, the default, leaves the output as it was.",
		Check: func(v string) error {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "text", "json":
				return nil
			}
			return fmt.Errorf("must be text or json")
		},
	})
	envLogLevel = env.Declare(env.Var{
		Name: "LOG_LEVEL", Kind: env.KindString, Default: "info",
		Doc: "Minimum level when LOG_FORMAT=json: debug, info, warn or error.",
		Check: func(v string) error {
			if _, ok := parseLevel(v); !ok {
				return fmt.Errorf("must be debug, info, warn or error")
			}
			return nil
		},
	})
)

// WithStructuredLogs switches the process to structured JSON logs (logx) when the
// server is created, whatever LOG_FORMAT says. It affects the whole process: the
// access log, slog's default logger and the standard log package.
func WithStructuredLogs() Option { return func(s *Server) { s.structuredLogs = true } }

// withLogOutput sends structured logs to w instead of stdout, for tests.
func withLogOutput(w io.Writer) Option { return func(s *Server) { s.logOutput = w } }

func parseLevel(v string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// installLogs makes logx the process logger when asked to, by WithStructuredLogs
// or LOG_FORMAT=json. Otherwise it does nothing, so a service's output is only
// ever changed by choice.
func (s *Server) installLogs() {
	if !s.structuredLogs && !strings.EqualFold(strings.TrimSpace(envLogFormat.Value()), "json") {
		return
	}
	level, _ := parseLevel(envLogLevel.Value()) // an invalid LOG_LEVEL falls back to info; the start-up check reports it
	l := logx.Install(logx.Options{
		Service: s.Config.AppName,
		Version: s.Config.Version,
		Node:    graph.Node(),
		Level:   level,
		Output:  s.logOutput,
	})
	middleware.SetLogger(l)
}

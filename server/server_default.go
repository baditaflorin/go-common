package server

import (
	"time"
)

// DefaultMaxBodyBytes is the default request body size limit applied
// by server.New unless overridden with WithMaxBodyBytes.
// 4 MiB matches the validate.Bind default and common reverse-proxy limits.
const DefaultMaxBodyBytes = 4 << 20

// DefaultDrainTimeout is how long Start waits for in-flight requests
// after receiving SIGTERM before forcing closure.
const DefaultDrainTimeout = 30 * time.Second

// Default http.Server timeouts applied by Start. WriteTimeout is
// overridable per-service via WithWriteTimeout for services that
// legitimately produce slow responses (e.g. a cold JS render).
const (
	DefaultReadTimeout  = 10 * time.Second
	DefaultWriteTimeout = 30 * time.Second
	DefaultIdleTimeout  = 120 * time.Second
)

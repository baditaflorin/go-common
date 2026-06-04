package safehttp

import (
	"time"
)

const breakerStateVersion = 1

// breakerStateConfig holds the knobs for WithPersistentBreakerState.
type breakerStateConfig struct {
	path            string
	persistInterval time.Duration
	saveOnShutdown  bool
}

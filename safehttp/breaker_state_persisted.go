package safehttp

import (
	"time"
)

// persistedEndpoint is the on-disk shape of a single host entry.
// Field tags are stable across versions; new fields must default to
// the v1-compatible zero value.
type persistedEndpoint struct {
	Host              string    `json:"host"`
	Status            int       `json:"status"`
	RetryAfterSeconds int       `json:"retry_after_seconds"`
	TS                time.Time `json:"ts"`
}

// persistedState is the on-disk file shape.
type persistedState struct {
	Version   int                 `json:"version"`
	SavedAt   time.Time           `json:"saved_at"`
	Endpoints []persistedEndpoint `json:"endpoints"`
}

package selftest

import (
	"context"
)

// CheckFunc is the signature every selftest check satisfies. A nil
// return is a pass; a non-nil error message ends up in the response.
// The context passed in is a child of the request context with the
// suite's per-check timeout applied — honor it.
type CheckFunc func(ctx context.Context) error

// CheckResult is one row in the JSON response. Err is empty when
// Pass is true.
type CheckResult struct {
	Name     string   `json:"name"`
	Category Category `json:"category,omitempty"`
	Pass     bool     `json:"pass"`
	Err      string   `json:"err,omitempty"`
}

// Package testclock re-exports clock.NewMock and clock.Mock for use in
// tests without importing the full clock package. This is a thin wrapper
// that makes the import path read clearly:
//
//	import "github.com/baditaflorin/go-common/testhelpers/testclock"
//	mc := testclock.New(time.Unix(0, 0))
//	mc.Advance(5 * time.Minute)
package testclock

import (
	"time"

	"github.com/baditaflorin/go-common/clock"
)

// Mock is an alias for clock.Mock for convenient import.
type Mock = clock.Mock

// New returns a new controllable mock clock set to start.
func New(start time.Time) *clock.Mock {
	return clock.NewMock(start)
}

// Epoch returns a Mock set to 2024-01-01T00:00:00Z, useful as a
// consistent zero-value for tests.
func Epoch() *clock.Mock {
	return clock.NewMock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
}

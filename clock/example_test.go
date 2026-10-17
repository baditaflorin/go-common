package clock_test

import (
	"fmt"
	"time"

	"github.com/baditaflorin/go-common/clock"
)

// Code that takes a clock.Clock can be driven by a Mock in tests.
func ExampleNewMock() {
	m := clock.NewMock(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	start := m.Now()
	m.Advance(90 * time.Second)
	fmt.Println(m.Since(start))
	// Output: 1m30s
}

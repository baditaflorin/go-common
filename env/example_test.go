package env_test

import (
	"fmt"
	"os"
	"time"

	"github.com/baditaflorin/go-common/env"
)

// Typed readers with a default replace the strconv boilerplate; an unset or
// unparsable variable falls back to the default.
func Example() {
	_ = os.Setenv("EXAMPLE_PORT", "8080")
	_ = os.Setenv("EXAMPLE_VERBOSE", "true")
	_ = os.Setenv("EXAMPLE_TIMEOUT", "250ms")
	defer func() {
		_ = os.Unsetenv("EXAMPLE_PORT")
		_ = os.Unsetenv("EXAMPLE_VERBOSE")
		_ = os.Unsetenv("EXAMPLE_TIMEOUT")
	}()

	fmt.Println(env.Int("EXAMPLE_PORT", 80))
	fmt.Println(env.Bool("EXAMPLE_VERBOSE", false))
	fmt.Println(env.Duration("EXAMPLE_TIMEOUT", 5*time.Second))
	fmt.Println(env.String("EXAMPLE_NOT_SET", "fallback"))
	// Output:
	// 8080
	// true
	// 250ms
	// fallback
}

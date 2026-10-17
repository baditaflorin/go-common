package nullify_test

import (
	"fmt"

	"github.com/baditaflorin/go-common/nullify"
)

// Store NULL, not the string "unknown", when a check found nothing.
func ExampleStringOrNil() {
	fmt.Println(nullify.StringOrNil("N/A") == nil)
	fmt.Println(*nullify.StringOrNil("  nginx "))
	// Output:
	// true
	// nginx
}

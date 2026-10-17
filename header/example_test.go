package header_test

import (
	"fmt"
	"net/http"

	"github.com/baditaflorin/go-common/header"
)

// Use the constants, not string literals: one typo in an auth header name is a
// silent bypass.
func Example() {
	h := http.Header{}
	h.Set(header.AuthUser, "svc-a")
	fmt.Println(header.AuthUser, h.Get(header.AuthUser))
	// Output: X-Auth-User svc-a
}

package pagination_test

import (
	"fmt"
	"net/http/httptest"

	"github.com/baditaflorin/go-common/pagination"
)

// Limits are clamped to MaxLimit; a cursor takes precedence over an offset.
func ExampleParseParams() {
	req := httptest.NewRequest("GET", "/items?limit=500&offset=40", nil)
	p := pagination.ParseParams(req)
	fmt.Println(p.Limit, p.Offset)
	// Output: 200 40
}

// Cursors are opaque base64url strings clients cannot usefully edit.
func ExampleEncodeCursor() {
	c := pagination.EncodeCursor("42")
	id, ok := pagination.DecodeCursor(c)
	fmt.Println(c, id, ok)
	// Output: NDI 42 true
}

package errors_test

import (
	stderrors "errors"
	"fmt"
	"net/http"

	fleeterrors "github.com/baditaflorin/go-common/errors"
)

// A typed error carries the HTTP status and a stable code next to the message.
// Clients match on the code, never on the message.
func ExampleNew() {
	err := fleeterrors.New(http.StatusNotFound, "not_found", "no such item")
	fmt.Println(err)
	fmt.Println(err.HTTPStatus())
	// Output:
	// not_found: no such item
	// 404
}

// Wrap keeps the cause reachable for errors.Is and errors.As.
func ExampleWrap() {
	cause := stderrors.New("connection refused")
	err := fleeterrors.Wrap(cause, http.StatusBadGateway, "upstream.unreachable", "fetch failed")
	fmt.Println(err)
	fmt.Println(stderrors.Is(err, cause))
	// Output:
	// upstream.unreachable: fetch failed: connection refused
	// true
}

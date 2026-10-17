package validate_test

import (
	"fmt"

	"github.com/baditaflorin/go-common/validate"
)

type createReq struct {
	Name string `json:"name" validate:"required,max=8"`
	Role string `json:"role" validate:"oneof=admin|user"`
}

// BindBytes decodes JSON and checks the validate tags in one step; every
// failure comes back as a single 400 error.
func ExampleBindBytes() {
	var ok createReq
	fmt.Println(validate.BindBytes([]byte(`{"name":"ada","role":"admin"}`), &ok) == nil)

	var bad createReq
	err := validate.BindBytes([]byte(`{"name":"","role":"root"}`), &bad)
	fmt.Println(err.HTTPStatus(), err.Code)
	// Output:
	// true
	// 400 bad_request.validation
}

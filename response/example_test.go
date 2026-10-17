package response_test

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/baditaflorin/go-common/response"
)

// Every fleet endpoint answers in this envelope; errors carry a stable
// error_code next to the HTTP status.
func ExampleNewError() {
	body, _ := json.Marshal(response.NewError(http.StatusForbidden,
		"auth.scope_mismatch", "token scope does not cover this resource"))
	fmt.Println(string(body))
	// Output: {"status":"error","error":{"code":403,"error_code":"auth.scope_mismatch","message":"token scope does not cover this resource"}}
}

func ExampleSuccess() {
	body, _ := json.Marshal(response.Success(map[string]int{"count": 2}))
	fmt.Println(string(body))
	// Output: {"status":"success","data":{"count":2}}
}

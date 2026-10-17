package validate_test

import (
	"regexp"
	"testing"

	"github.com/baditaflorin/go-common/validate"
)

type fuzzReq struct {
	Name   string   `json:"name" validate:"required,max=64"`
	Email  string   `json:"email" validate:"email"`
	URL    string   `json:"url" validate:"url"`
	Role   string   `json:"role" validate:"oneof=admin|user|viewer"`
	Code   string   `json:"code" validate:"pattern=[A-Z]{2}[0-9]{3}"`
	Age    int      `json:"age" validate:"min=0,max=150"`
	Score  float64  `json:"score" validate:"min=0,max=1"`
	Tags   []string `json:"tags"`
	Nested struct {
		Inner string `json:"inner" validate:"required"`
	} `json:"nested"`
}

var fuzzCode = regexp.MustCompile(`^(?:[A-Z]{2}[0-9]{3})$`)

// FuzzBindBytes decodes arbitrary JSON into a struct that uses every tag. It
// must not panic, and anything it accepts must actually satisfy the tags.
func FuzzBindBytes(f *testing.F) {
	f.Add([]byte(`{"name":"a","nested":{"inner":"x"}}`))
	f.Add([]byte(`{"name":"a","email":"a@b.co","url":"https://x.io","role":"admin","code":"AB123","age":3,"score":0.5,"nested":{"inner":"x"}}`))
	f.Add([]byte(`{"name":"","role":"root","code":"zz","age":-1,"score":2}`))
	f.Add([]byte(`{"unknown":1}`))
	f.Add([]byte(`[1,2,3]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var req fuzzReq
		if err := validate.BindBytes(data, &req); err != nil {
			return
		}
		if req.Name == "" {
			t.Fatalf("accepted empty required Name: %q", data)
		}
		if req.Nested.Inner == "" {
			t.Fatalf("accepted empty required Nested.Inner: %q", data)
		}
		switch req.Role {
		case "", "admin", "user", "viewer":
		default:
			t.Fatalf("accepted role %q outside oneof: %q", req.Role, data)
		}
		if req.Code != "" && !fuzzCode.MatchString(req.Code) {
			t.Fatalf("accepted code %q that fails the anchored pattern: %q", req.Code, data)
		}
		if again := validate.Struct(&req); again != nil {
			t.Fatalf("Struct disagrees with BindBytes on %q: %v", data, again)
		}
	})
}

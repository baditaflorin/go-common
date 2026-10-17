package validate_test

import (
	"testing"

	"github.com/baditaflorin/go-common/validate"
)

type benchReq struct {
	Name  string `json:"name" validate:"required,max=64"`
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role" validate:"oneof=admin|user|viewer"`
	Age   int    `json:"age" validate:"min=0,max=150"`
}

func BenchmarkBindBytes(b *testing.B) {
	body := []byte(`{"name":"Ada Lovelace","email":"ada@example.com","role":"admin","age":36}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var req benchReq
		if err := validate.BindBytes(body, &req); err != nil {
			b.Fatal(err)
		}
	}
}

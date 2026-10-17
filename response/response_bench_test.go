package response_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/response"
)

func BenchmarkEnvelope(b *testing.B) {
	data := map[string]any{"id": 42, "name": "example.com", "tags": []string{"a", "b"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = response.Envelope(data, 1)
	}
}

func BenchmarkSuccessMarshal(b *testing.B) {
	r := response.Success(map[string]any{"id": 42, "name": "example.com"})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewErrorMarshal(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := response.NewError(http.StatusForbidden, "auth.scope_mismatch", "token scope does not cover this resource")
		if _, err := json.Marshal(r); err != nil {
			b.Fatal(err)
		}
	}
}

package pagination_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/baditaflorin/go-common/pagination"
)

func BenchmarkParseParams(b *testing.B) {
	req := &http.Request{URL: &url.URL{RawQuery: "limit=50&cursor=NDI&offset=0"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = pagination.ParseParams(req)
	}
}

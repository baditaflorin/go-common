package pagination_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/baditaflorin/go-common/pagination"
)

// FuzzCursorRoundTrip: whatever a service encodes, it must get back.
func FuzzCursorRoundTrip(f *testing.F) {
	f.Add("abc")
	f.Add("2026-10-17T00:00:00Z|42")
	f.Add("\x00\xff\xfe")
	f.Fuzz(func(t *testing.T, s string) {
		if s == "" {
			return // EncodeCursor("") is "", which DecodeCursor treats as absent
		}
		got, ok := pagination.DecodeCursor(pagination.EncodeCursor(s))
		if !ok || got != s {
			t.Fatalf("round trip of %q gave %q, %v", s, got, ok)
		}
	})
}

// FuzzDecodeCursor: cursors come straight from the query string.
func FuzzDecodeCursor(f *testing.F) {
	f.Add("YWJj")
	f.Add("!!!")
	f.Add("YWJj=")
	f.Fuzz(func(t *testing.T, c string) {
		_, _ = pagination.DecodeCursor(c)
	})
}

// FuzzParseParams checks the clamping contract for any query string: the limit
// stays in 1..MaxLimit, the offset is never negative, and a cursor wins over
// an offset.
func FuzzParseParams(f *testing.F) {
	f.Add("limit=10&offset=5")
	f.Add("limit=-1&cursor=YWJj&offset=9")
	f.Add("limit=99999999999999999999")
	f.Add("limit=%00&offset=-3&cursor=%20")
	f.Fuzz(func(t *testing.T, rawQuery string) {
		req := &http.Request{URL: &url.URL{RawQuery: rawQuery}}
		p := pagination.ParseParams(req)
		if p.Limit < 1 || p.Limit > pagination.MaxLimit {
			t.Fatalf("limit %d outside 1..%d for %q", p.Limit, pagination.MaxLimit, rawQuery)
		}
		if p.Offset < 0 {
			t.Fatalf("negative offset %d for %q", p.Offset, rawQuery)
		}
		if p.Cursor != "" && p.Offset != 0 {
			t.Fatalf("offset %d set alongside cursor %q", p.Offset, p.Cursor)
		}
	})
}

// FuzzParseParamsWithDefaults: caller-chosen bounds must also hold.
func FuzzParseParamsWithDefaults(f *testing.F) {
	f.Add("limit=500", 100, 1000)
	f.Add("", 0, 0)
	f.Add("limit=3", -5, 7)
	f.Fuzz(func(t *testing.T, rawQuery string, defLimit, maxLimit int) {
		req := &http.Request{URL: &url.URL{RawQuery: rawQuery}}
		p := pagination.ParseParamsWithDefaults(req, defLimit, maxLimit)
		if p.Limit < 1 {
			t.Fatalf("limit %d < 1 (default=%d max=%d)", p.Limit, defLimit, maxLimit)
		}
		if maxLimit >= 1 && p.Limit > maxLimit {
			t.Fatalf("limit %d above max %d", p.Limit, maxLimit)
		}
	})
}

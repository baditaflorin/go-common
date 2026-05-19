package pagination_test

import (
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/pagination"
)

func TestParseParamsDefaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/items", nil)
	p := pagination.ParseParams(r)
	if p.Limit != pagination.DefaultLimit {
		t.Fatalf("limit: got %d, want %d", p.Limit, pagination.DefaultLimit)
	}
	if p.Cursor != "" {
		t.Fatalf("cursor: got %q, want empty", p.Cursor)
	}
	if p.Offset != 0 {
		t.Fatalf("offset: got %d, want 0", p.Offset)
	}
}

func TestParseParamsLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/items?limit=50", nil)
	p := pagination.ParseParams(r)
	if p.Limit != 50 {
		t.Fatalf("limit: got %d, want 50", p.Limit)
	}
}

func TestParseParamsLimitCapped(t *testing.T) {
	r := httptest.NewRequest("GET", "/items?limit=9999", nil)
	p := pagination.ParseParams(r)
	if p.Limit != pagination.MaxLimit {
		t.Fatalf("limit: got %d, want %d (capped)", p.Limit, pagination.MaxLimit)
	}
}

func TestParseParamsCursor(t *testing.T) {
	cursor := pagination.EncodeCursor("uuid-123")
	r := httptest.NewRequest("GET", "/items?cursor="+cursor, nil)
	p := pagination.ParseParams(r)
	if p.Cursor != cursor {
		t.Fatalf("cursor: got %q, want %q", p.Cursor, cursor)
	}
	if p.Offset != 0 {
		t.Fatal("cursor set: offset should be 0")
	}
}

func TestParseParamsOffset(t *testing.T) {
	r := httptest.NewRequest("GET", "/items?offset=40", nil)
	p := pagination.ParseParams(r)
	if p.Offset != 40 {
		t.Fatalf("offset: got %d, want 40", p.Offset)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	original := "user:abc-123:ts:1716000000"
	encoded := pagination.EncodeCursor(original)
	decoded, ok := pagination.DecodeCursor(encoded)
	if !ok {
		t.Fatal("DecodeCursor returned false")
	}
	if decoded != original {
		t.Fatalf("got %q, want %q", decoded, original)
	}
}

func TestDecodeCursorInvalid(t *testing.T) {
	_, ok := pagination.DecodeCursor("not!base64!!")
	if ok {
		t.Fatal("expected false for invalid cursor")
	}
}

func TestDecodeCursorEmpty(t *testing.T) {
	_, ok := pagination.DecodeCursor("")
	if ok {
		t.Fatal("expected false for empty cursor")
	}
}

func TestNewPage(t *testing.T) {
	items := []string{"a", "b", "c"}
	next := pagination.EncodeCursor("d")
	page := pagination.NewPage(items, next, 100)
	if len(page.Items) != 3 {
		t.Fatalf("items len: got %d, want 3", len(page.Items))
	}
	if !page.HasMore() {
		t.Fatal("expected HasMore=true")
	}
	if page.Total != 100 {
		t.Fatalf("total: got %d, want 100", page.Total)
	}
}

func TestNewPageLastPage(t *testing.T) {
	page := pagination.NewPage([]int{1, 2}, "", 2)
	if page.HasMore() {
		t.Fatal("expected HasMore=false on last page")
	}
}

func TestEmpty(t *testing.T) {
	page := pagination.Empty[string](20)
	if len(page.Items) != 0 {
		t.Fatal("Empty should have no items")
	}
	if page.Limit != 20 {
		t.Fatalf("limit: got %d, want 20", page.Limit)
	}
}

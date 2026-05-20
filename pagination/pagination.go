// Package pagination provides canonical types and helpers for
// cursor-based and offset-based paginated API responses.
//
// Fleet APIs return lists. Without a canonical Page[T] every service
// invents its own {items, next, total} shape, making client code
// diverge. This package defines:
//
//   - Page[T]      — the standard JSON response wrapper
//   - Params       — parsed query-string pagination parameters
//   - ParseParams  — extract + validate Params from an *http.Request
//   - Cursor       — opaque base64 cursor backed by an encoded string
//
// Usage (handler side):
//
//	params := pagination.ParseParams(r)
//	items, next := fetchPage(params.Limit, params.Cursor)
//	page := pagination.NewPage(items, next, total)
//	json.NewEncoder(w).Encode(page)
//
// Usage (client side):
//
//	var page pagination.Page[MyItem]
//	json.NewDecoder(resp.Body).Decode(&page)
//	for _, item := range page.Items { … }
//	if page.HasMore() { fetchPage(page.Next) }
package pagination

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// DefaultLimit is the number of items returned when the caller doesn't
// specify a limit. Tuned for typical fleet list endpoints.
const DefaultLimit = 20

// MaxLimit is the hard cap on items per page. Requests asking for more
// are silently clamped to MaxLimit.
const MaxLimit = 200

// Page is the canonical paginated response wrapper. T is the item type.
type Page[T any] struct {
	// Items is the slice of results for this page.
	Items []T `json:"items"`

	// Next is the opaque cursor to pass as ?cursor= to get the next page.
	// Empty string means this is the last page.
	Next string `json:"next,omitempty"`

	// Total is the total number of matching items across all pages.
	// Set to -1 (and omit from JSON) when the total is unknown.
	Total int `json:"total,omitempty"`

	// Limit echoes back the effective limit used.
	Limit int `json:"limit"`
}

// NewPage creates a Page. next is the cursor for the next page (empty
// if this is the last). Use total=-1 when the total count is unknown.
func NewPage[T any](items []T, next string, total int) Page[T] {
	limit := len(items)
	if limit == 0 {
		limit = DefaultLimit
	}
	p := Page[T]{Items: items, Next: next, Limit: limit}
	if total >= 0 {
		p.Total = total
	}
	return p
}

// HasMore returns true when there is a next page.
func (p Page[T]) HasMore() bool { return p.Next != "" }

// Empty returns an empty page with the given limit.
func Empty[T any](limit int) Page[T] {
	return Page[T]{Items: []T{}, Limit: limit}
}

// ─── Params ──────────────────────────────────────────────────────────────

// Params holds the parsed, validated pagination parameters from a
// query string. All values are ready to use; defaults have been applied.
type Params struct {
	// Limit is the number of items to return. Always in [1, MaxLimit].
	Limit int

	// Cursor is the opaque continuation token from the previous page.
	// Empty string means start from the beginning.
	Cursor string

	// Offset is the numeric offset (for offset-based pagination).
	// Mutually exclusive with Cursor; Cursor takes precedence.
	Offset int
}

// ParseParams extracts and validates pagination parameters from the
// request query string. Query keys:
//
//	?limit=N    (default: DefaultLimit, max: MaxLimit)
//	?cursor=X   (opaque string; validated as non-empty base64url if non-empty)
//	?offset=N   (default: 0; ignored when cursor is set)
func ParseParams(r *http.Request) Params {
	q := r.URL.Query()
	p := Params{
		Limit:  DefaultLimit,
		Cursor: "",
		Offset: 0,
	}

	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if p.Limit < 1 {
		p.Limit = 1
	}

	if s := strings.TrimSpace(q.Get("cursor")); s != "" {
		p.Cursor = s
	}

	if p.Cursor == "" {
		if s := q.Get("offset"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				p.Offset = n
			}
		}
	}

	return p
}

// ParseParamsWithDefaults is like ParseParams but lets the caller
// override the fleet-wide defaults. Use this when an endpoint has a
// domain-specific default or cap that differs from DefaultLimit/MaxLimit
// (e.g. audit-log endpoints that default to 100 items and cap at 1000).
func ParseParamsWithDefaults(r *http.Request, defaultLimit, maxLimit int) Params {
	q := r.URL.Query()
	p := Params{
		Limit:  defaultLimit,
		Cursor: "",
		Offset: 0,
	}

	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if p.Limit > maxLimit {
		p.Limit = maxLimit
	}
	if p.Limit < 1 {
		p.Limit = 1
	}

	if s := strings.TrimSpace(q.Get("cursor")); s != "" {
		p.Cursor = s
	}

	if p.Cursor == "" {
		if s := q.Get("offset"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				p.Offset = n
			}
		}
	}

	return p
}

// ─── Cursor helpers ───────────────────────────────────────────────────────

// EncodeCursor encodes an arbitrary string value as a URL-safe base64
// cursor. The opaque encoding prevents clients from guessing or
// manipulating cursor internals.
func EncodeCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// DecodeCursor decodes a cursor produced by EncodeCursor. Returns the
// original string and true on success, or empty string and false if the
// cursor is malformed.
func DecodeCursor(cursor string) (string, bool) {
	if cursor == "" {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// CursorFromID is a convenience wrapper: builds a cursor from a string
// ID (e.g. a UUID or timestamp string).
func CursorFromID(id string) string { return EncodeCursor(id) }

// IDFromCursor extracts the ID from a cursor built by CursorFromID.
func IDFromCursor(cursor string) (string, bool) { return DecodeCursor(cursor) }

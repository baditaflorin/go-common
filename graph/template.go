package graph

import (
	"net/url"
	"strconv"
	"strings"
)

// templatisePath collapses URL paths to stable templates so the
// collector doesn't see per-call cardinality blowup.
//
//	/users/42        → /users/{id}
//	/share/<uuid>    → /share/{uuid}
//	/t/<32-hex>/foo  → /t/{token}/foo
//
// Conservative: anything it doesn't recognise is left alone, so
// /api/v2/orders stays /api/v2/orders. Strips query string entirely.
//
// This duplicates client.templatisePath intentionally — client imports
// safehttp which imports graph, so promoting the helper there would
// create a cycle. Kept identical so the two stay in sync.
func templatisePath(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	// Strip query string and fragment.
	if i := strings.IndexAny(rawPath, "?#"); i >= 0 {
		rawPath = rawPath[:i]
	}
	// Try to parse as a URL first in case we were handed a full URL.
	if strings.Contains(rawPath, "://") {
		if u, err := url.Parse(rawPath); err == nil {
			rawPath = u.Path
		}
	}
	if rawPath == "" {
		return "/"
	}
	parts := strings.Split(rawPath, "/")
	for i, p := range parts {
		switch {
		case p == "":
			continue
		case looksLikeUUID(p):
			parts[i] = "{uuid}"
		case looksLikeInt(p):
			parts[i] = "{id}"
		case looksLikeOpaqueToken(p):
			parts[i] = "{token}"
		}
	}
	return strings.Join(parts, "/")
}

func looksLikeInt(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func looksLikeOpaqueToken(s string) bool {
	if len(s) < 24 {
		return false
	}
	digits, letters := 0, 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letters++
		case c == '-' || c == '_' || c == '.':
			// JWT delimiter + base64url filler
		default:
			return false
		}
	}
	// Need a meaningful mix of digits + letters to look "token-y".
	return digits > 0 && letters > 0
}

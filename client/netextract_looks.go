package client

import (
	"net/url"
	"strconv"
	"strings"
)

func looksLikeInt(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func looksLikeUUID(s string) bool {
	// 8-4-4-4-12 hex with dashes. Cheap shape check.
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

// looksLikeOpaqueToken flags long hex/base64-looking segments. Tuned
// for SHA-1 / SHA-256 hex digests and JWT-style tokens that appear in
// URL paths (e.g. /share/<long-hash>). Threshold is intentionally
// high so we don't trample on slugs.
func looksLikeOpaqueToken(s string) bool {
	if len(s) < 24 {
		return false
	}
	digits, letters, other := 0, 0, 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letters++
		case c == '-' || c == '_' || c == '.':
			// JWT delimiter + base64url filler
		default:
			other++
		}
	}
	if other > 0 {
		return false
	}
	// Require both digits and letters present, like a real hash/token.
	return digits > 0 && letters > 0 && digits+letters >= 24
}

func looksLikeJS(rawURL string, headers map[string]string) bool {
	ct := strings.ToLower(headerLookup(headers, "Content-Type"))
	if strings.HasPrefix(ct, "application/javascript") ||
		strings.HasPrefix(ct, "text/javascript") ||
		strings.HasPrefix(ct, "application/x-javascript") ||
		strings.HasPrefix(ct, "module") {
		return true
	}
	// Fall back to path extension when proxy strips Content-Type.
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".js") ||
		strings.HasSuffix(p, ".mjs") ||
		strings.HasSuffix(p, ".cjs")
}

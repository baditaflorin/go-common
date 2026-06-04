package client

import (
	"net/url"
	"strings"
)

// ByResourceType keeps entries whose resource_type matches any of the
// given values (case-insensitive). Common values: "document",
// "script", "stylesheet", "image", "xhr", "fetch", "websocket",
// "font", "media", "manifest".
func ByResourceType(entries []NetworkEntry, types ...string) []NetworkEntry {
	want := make(map[string]struct{}, len(types))
	for _, t := range types {
		want[strings.ToLower(t)] = struct{}{}
	}
	out := []NetworkEntry{}
	for _, e := range entries {
		if _, hit := want[strings.ToLower(e.ResourceType)]; hit {
			out = append(out, e)
		}
	}
	return out
}

// ByHostSuffix keeps entries whose URL host ends with any of the
// given suffixes (case-insensitive). Use to split "first-party vs
// third-party" without writing your own URL parsing.
func ByHostSuffix(entries []NetworkEntry, suffixes ...string) []NetworkEntry {
	suff := make([]string, len(suffixes))
	for i, s := range suffixes {
		suff[i] = strings.ToLower(s)
	}
	out := []NetworkEntry{}
	for _, e := range entries {
		u, err := url.Parse(e.URL)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Host)
		for _, s := range suff {
			if strings.HasSuffix(host, s) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// ByMethod keeps entries with one of the listed HTTP methods.
func ByMethod(entries []NetworkEntry, methods ...string) []NetworkEntry {
	want := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		want[strings.ToUpper(m)] = struct{}{}
	}
	out := []NetworkEntry{}
	for _, e := range entries {
		if _, hit := want[strings.ToUpper(e.Method)]; hit {
			out = append(out, e)
		}
	}
	return out
}

// ByStatusClass keeps entries whose status code matches one of the
// listed class digits (1, 2, 3, 4, 5). Cheap shortcut for "show me
// every redirect" (ByStatusClass(_, 3)) or "every error"
// (ByStatusClass(_, 4, 5)).
func ByStatusClass(entries []NetworkEntry, classes ...int) []NetworkEntry {
	want := make(map[int]struct{}, len(classes))
	for _, c := range classes {
		want[c] = struct{}{}
	}
	out := []NetworkEntry{}
	for _, e := range entries {
		if _, hit := want[e.Status/100]; hit {
			out = append(out, e)
		}
	}
	return out
}

// BySizeGreaterThan keeps entries whose response_size exceeds n bytes.
// Useful when iterating JSAssets to skip tiny stubs/empty chunks.
func BySizeGreaterThan(entries []NetworkEntry, n int64) []NetworkEntry {
	out := []NetworkEntry{}
	for _, e := range entries {
		if e.ResponseSize > n {
			out = append(out, e)
		}
	}
	return out
}

// ByContentType keeps entries whose response Content-Type header
// begins with any of the listed prefixes (case-insensitive). Pass
// e.g. "application/javascript", "text/javascript".
func ByContentType(entries []NetworkEntry, prefixes ...string) []NetworkEntry {
	pref := make([]string, len(prefixes))
	for i, p := range prefixes {
		pref[i] = strings.ToLower(p)
	}
	out := []NetworkEntry{}
	for _, e := range entries {
		ct := strings.ToLower(headerLookup(e.ResponseHeaders, "Content-Type"))
		for _, p := range pref {
			if strings.HasPrefix(ct, p) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

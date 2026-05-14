// Package client - netextract.go consumes the rich telemetry that
// JSProxy / Fetch(WithNetworkLog(true)) attach to FetchResult.Network
// and turns it into the small set of primitives that pentest / recon
// services actually want:
//
//	XHREndpoints       — every API call the SPA made (xhr+fetch+ws),
//	                     deduped by method+path-template. The
//	                     "attack-surface map" output that idor-finder,
//	                     graphql-introspection, cors-scanner, and
//	                     api-extractor all want to consume.
//	GraphQLEndpoints   — XHR/fetch POSTs whose payload looks like a
//	                     GraphQL operation. Cheap structural sniff;
//	                     downstream services run real introspection.
//	JSAssets           — every loaded script (.js, modulepreload, eval
//	                     blobs). secrets-scanner and apikey-scanner
//	                     iterate this list to fetch+entropy-scan each.
//	SourcemapCandidates — JSAssets whose response advertises a
//	                     SourceMap header or a /*# sourceMappingURL=*/
//	                     marker via initiator chain. sourcemap-finder.
//	IframeURLs         — child documents loaded into <iframe>. Frame
//	                     embeddability analysis (clickjacking,
//	                     iframe-analyzer).
//	RedirectParams     — query-string keys observed across the network
//	                     that look like redirect targets (?next=,
//	                     ?return_to=, ?redirect_uri=, ...). Seed for
//	                     open-redirect fuzzing.
//	SetCookiesAll      — every Set-Cookie observed across all hops
//	                     (not just the final document). Auth/session
//	                     analysis (session-management, cookie-checker).
//	ConsoleErrors      — console log lines that look like errors or
//	                     framework dev-mode warnings. debug-detector.
//	By                 — generic filters: ByResourceType, ByHostSuffix,
//	                     ByMethod, ByStatusClass, BySizeGreaterThan,
//	                     ByContentType. Compose freely.
//
// The helpers never panic on nil input — passing a zero FetchResult
// returns empty slices, so callers can wire them in unconditionally.
//
// All extractors are pure functions of the rich-telemetry payload.
// They do NOT make new network calls — that's the consumer service's
// job. This keeps the helpers cheap, deterministic, and trivially
// testable.
package client

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// XHREndpoint is one runtime-observed API call.
type XHREndpoint struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type,omitempty"`
	Initiator    string `json:"initiator,omitempty"`
	ResponseSize int64  `json:"response_size"`
}

// XHREndpoints returns one entry per (method, normalised-URL) seen as
// XHR/fetch/WebSocket traffic in the rendered network log. URL
// normalisation collapses numeric and UUID path segments to {id}/{uuid}
// so /users/42, /users/43, /users/44 dedupe to a single
// /users/{id} template — that's the surface a tampering tool wants.
//
// Returns empty slice for nil/empty input.
func XHREndpoints(r *FetchResult) []XHREndpoint {
	if r == nil || len(r.Network) == 0 {
		return nil
	}
	seen := make(map[string]XHREndpoint, len(r.Network))
	for _, e := range r.Network {
		switch strings.ToLower(e.ResourceType) {
		case "xhr", "fetch", "websocket":
		default:
			continue
		}
		tmpl := templatisePath(e.URL)
		key := strings.ToUpper(e.Method) + " " + tmpl
		// Keep the first observation for each (method, template). Later
		// duplicates are usually the same call with different IDs.
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = XHREndpoint{
			URL:          tmpl,
			Method:       strings.ToUpper(e.Method),
			Status:       e.Status,
			ContentType:  headerLookup(e.ResponseHeaders, "Content-Type"),
			Initiator:    e.Initiator,
			ResponseSize: e.ResponseSize,
		}
	}
	out := make([]XHREndpoint, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// GraphQLEndpoints returns endpoints likely speaking GraphQL: an
// XHR/fetch POST whose URL contains "graphql" OR whose Content-Type
// is application/json AND ResponseSize > 0. The cheap heuristic is
// intentional — downstream graphql-introspection probes /graphql with
// a real introspection query to confirm.
func GraphQLEndpoints(r *FetchResult) []XHREndpoint {
	out := []XHREndpoint{}
	for _, e := range XHREndpoints(r) {
		if e.Method != "POST" {
			continue
		}
		if strings.Contains(strings.ToLower(e.URL), "graphql") ||
			strings.HasPrefix(strings.ToLower(e.ContentType), "application/json") {
			out = append(out, e)
		}
	}
	return out
}

// JSAssets returns every script the browser loaded — both static
// <script src> and dynamically imported chunks. Used by
// secrets-scanner / apikey-scanner / sourcemap-finder as the input
// list to re-fetch and inspect.
//
// Resource types matched: "script", "stylesheet" is intentionally
// excluded (CSS rarely carries secrets). Eval/data-URL chunks are
// kept — they often host inline credentials.
func JSAssets(r *FetchResult) []NetworkEntry {
	if r == nil {
		return nil
	}
	out := make([]NetworkEntry, 0, len(r.Network))
	for _, e := range r.Network {
		rt := strings.ToLower(e.ResourceType)
		if rt != "script" {
			continue
		}
		if !looksLikeJS(e.URL, e.ResponseHeaders) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SourcemapCandidates returns JSAssets whose response advertises a
// SourceMap (case-insensitive header `SourceMap:` or
// `X-SourceMap:`). The actual `//# sourceMappingURL=` marker lives in
// the JS body — sourcemap-finder still re-fetches; this just narrows
// the candidate set from "everything" to "things the server already
// admits have a map."
func SourcemapCandidates(r *FetchResult) []NetworkEntry {
	if r == nil {
		return nil
	}
	out := []NetworkEntry{}
	for _, e := range JSAssets(r) {
		if headerLookup(e.ResponseHeaders, "SourceMap") != "" ||
			headerLookup(e.ResponseHeaders, "X-SourceMap") != "" {
			out = append(out, e)
		}
	}
	return out
}

// IframeURLs returns the URLs of every child document the page loaded
// into an iframe (CDP resource_type "document" with an initiator that
// isn't the top-level navigation).
func IframeURLs(r *FetchResult) []string {
	if r == nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, e := range r.Network {
		if !strings.EqualFold(e.ResourceType, "document") {
			continue
		}
		if e.URL == "" || e.URL == r.FinalURL {
			continue
		}
		if _, dup := seen[e.URL]; dup {
			continue
		}
		seen[e.URL] = struct{}{}
		out = append(out, e.URL)
	}
	return out
}

// RedirectParam is one observed query parameter on a real network
// request whose name pattern matches a known open-redirect sink.
type RedirectParam struct {
	OnURL string `json:"on_url"` // request URL where the param appeared
	Key   string `json:"key"`    // the parameter name (e.g. "next")
	Value string `json:"value"`  // the observed value (may itself be a URL)
}

// redirectParamKeys is the curated list of query keys that empirically
// turn out to be redirect sinks. Kept conservative to keep the false
// positive rate low — open-redirect fuzzing is expensive.
var redirectParamKeys = []string{
	"next", "return", "return_to", "returnto", "return_url", "returnurl",
	"redirect", "redirect_uri", "redirecturi", "redirect_url", "redirecturl",
	"continue", "continue_to", "url", "destination", "dest",
	"target", "callback", "cb", "back", "backurl", "ref", "go", "goto",
	"r", "u", "uri", "site",
}

// RedirectParams scans every recorded network URL (not just the
// rendered page) and returns each query parameter whose key matches a
// known redirect sink. Use this as the seed list for fuzzing instead
// of guessing parameter names from corpora.
func RedirectParams(r *FetchResult) []RedirectParam {
	if r == nil {
		return nil
	}
	keys := make(map[string]struct{}, len(redirectParamKeys))
	for _, k := range redirectParamKeys {
		keys[k] = struct{}{}
	}
	out := []RedirectParam{}
	seen := map[string]struct{}{}
	for _, e := range r.Network {
		u, err := url.Parse(e.URL)
		if err != nil || u.RawQuery == "" {
			continue
		}
		for k, vs := range u.Query() {
			if _, hit := keys[strings.ToLower(k)]; !hit {
				continue
			}
			for _, v := range vs {
				dedupe := e.URL + "?" + k + "=" + v
				if _, dup := seen[dedupe]; dup {
					continue
				}
				seen[dedupe] = struct{}{}
				out = append(out, RedirectParam{OnURL: e.URL, Key: k, Value: v})
			}
		}
	}
	return out
}

// SetCookiesAll returns every Set-Cookie header observed across every
// hop of the network — not just the final document. Auth flows that
// rotate tokens on /login or /oauth/callback are invisible to a
// "what's the cookie on the landing page" check; this surfaces them.
//
// Each entry is the raw header line plus the URL that issued it. We
// keep it as []FetchCookie for symmetry with FetchResult.CookiesSet,
// using only the Raw + SetOn fields — full attribute parsing belongs
// in cookie-checker.
func SetCookiesAll(r *FetchResult) []FetchCookie {
	if r == nil {
		return nil
	}
	out := []FetchCookie{}
	for _, e := range r.Network {
		raw := headerLookup(e.ResponseHeaders, "Set-Cookie")
		if raw == "" {
			continue
		}
		// Some browser captures join multiple Set-Cookie with comma;
		// preserve as-is, downstream cookie-checker splits properly.
		if c := parseSetCookie(raw, e.URL); c != nil {
			out = append(out, *c)
		}
	}
	// Also keep anything the proxy already attached at the top level.
	for _, c := range r.CookiesSet {
		out = append(out, c)
	}
	return out
}

// ConsoleErrors filters ConsoleLogs to lines that look like errors or
// dev-mode warnings. Cheap substring sniff — debug-detector applies
// stricter classification.
func ConsoleErrors(r *FetchResult) []string {
	if r == nil {
		return nil
	}
	out := []string{}
	for _, line := range r.ConsoleLogs {
		l := strings.ToLower(line)
		if strings.Contains(l, "error") ||
			strings.Contains(l, "warning") ||
			strings.Contains(l, "uncaught") ||
			strings.Contains(l, "unhandled") ||
			strings.Contains(l, "development mode") ||
			strings.Contains(l, "devtools") {
			out = append(out, line)
		}
	}
	return out
}

// ---- generic filters ------------------------------------------------

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

// ---- internals ------------------------------------------------------

// templatisePath collapses /users/42 → /users/{id}, UUIDs → {uuid},
// long opaque hex/base64 → {token}. Conservative: leaves anything it
// doesn't recognise alone so /api/v2/orders stays /api/v2/orders.
func templatisePath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parts := strings.Split(u.Path, "/")
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
	// Build the templated URL by hand: url.URL.String() percent-encodes
	// `{` and `}` because RFC 3986 considers them sub-delim reserved.
	// Templates are display strings, not real requests, so keep them
	// literal.
	scheme := u.Scheme
	host := u.Host
	if scheme == "" && host == "" {
		return strings.Join(parts, "/")
	}
	return scheme + "://" + host + strings.Join(parts, "/")
}

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

// headerLookup does a case-insensitive lookup against the flat header
// map. Browser captures don't normalise casing, so direct map access
// is unreliable.
func headerLookup(h map[string]string, key string) string {
	if v, ok := h[key]; ok {
		return v
	}
	lk := strings.ToLower(key)
	for k, v := range h {
		if strings.ToLower(k) == lk {
			return v
		}
	}
	return ""
}

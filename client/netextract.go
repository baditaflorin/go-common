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
	"strings"
)

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

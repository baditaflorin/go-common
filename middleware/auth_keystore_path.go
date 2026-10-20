package middleware

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// The keystore gate decides whether a request is exempt from authentication from
// its path, before any router sees it, and the router then picks a handler from
// the path again. The two decisions agree only if both read the same string, and
// routers do not: net/http.ServeMux redirects a path with dot segments, a router
// that calls path.Clean before routing serves the cleaned path, one that decodes
// the path a second time reads %2e%2e as "..", and one that routes on the raw
// form treats %2f as an ordinary character. A path that starts with a public
// prefix as written can therefore name a different route once a router has
// normalised it.
//
// The rule that keeps the gate out of that question: a path is exempt only if it
// is canonical, that is, no reading of it leads anywhere but the route the gate
// saw. A request that names an exempt path in any other form is not forwarded and
// not authenticated: it is refused with 400, see denyBadPath.

// publicPathVerdict is what the gate concludes from a request path alone.
type publicPathVerdict uint8

const (
	// pathNotPublic: the path names no exempt route. The request goes through the
	// credential checks like any other.
	pathNotPublic publicPathVerdict = iota
	// pathPublic: an exempt route, spelled canonically. The credential checks are
	// skipped.
	pathPublic
	// pathAmbiguous: the path starts like an exempt route but is not canonical.
	pathAmbiguous
)

// isFixedPublicPath reports whether p is one of the paths every keystore-gated
// fleet service answers without a key: the fleet contract's probes and its
// discovery documents.
func isFixedPublicPath(p string) bool {
	switch p {
	case "/health", "/version", "/selftest", "/_gw_health", "/capabilities", "/openapi.json", "/agent.json":
		return true
	}
	return false
}

// usablePublicPrefix reports whether a declared prefix counts: "" and "/" never
// do, because either would make every route public by a configuration mistake.
func usablePublicPrefix(p string) bool { return p != "" && p != "/" }

// MatchPublicPathPrefix reports the first of prefixes that makes
// KeystoreOpts.PublicPathPrefixes treat path as public, and whether there is one.
// It is the gate's own prefix test, a plain prefix match in which "" and "/" never
// match, exposed so that a router can check its routes against the rule the gate
// applies instead of keeping a second copy of it. It looks only at the string it
// is given: the gate also requires the request path to be canonical, see
// KeystoreOpts.PublicPathPrefixes.
func MatchPublicPathPrefix(path string, prefixes []string) (prefix string, ok bool) {
	for _, p := range prefixes {
		if usablePublicPrefix(p) && strings.HasPrefix(path, p) {
			return p, true
		}
	}
	return "", false
}

// publicPrefix is a declared prefix, with the text the raw path must start with
// when a request matches it. The two are the same for every prefix in use. A
// prefix with a character that needs escaping (a space, a non-ASCII letter) has
// no single raw spelling, since the hex digits of an escape may be either case,
// so for it rawPrefix is empty and only the decoded path is compared.
type publicPrefix struct{ decoded, rawPrefix string }

func newPublicPrefixes(prefixes []string) []publicPrefix {
	var out []publicPrefix
	for _, p := range prefixes {
		if !usablePublicPrefix(p) {
			continue
		}
		pre := publicPrefix{decoded: p}
		if (&url.URL{Path: p}).EscapedPath() == p {
			pre.rawPrefix = p
		}
		out = append(out, pre)
	}
	return out
}

// classifyPublicPath decides whether the request with this URL is exempt.
//
// The exempt test is the one the gate always had: the decoded path is one of the
// fixed paths, or starts with a declared prefix. What is new is that the answer
// is "exempt" only for a canonical path, and only if the raw form agrees with the
// decoded one about what it matched (a request for /%63/x decodes to /c/x, and a
// router that routes on the raw path does not see /c/ there). A path that passes
// the old test but not these is pathAmbiguous; one that passes none of it is
// not affected at all.
func classifyPublicPath(u *url.URL, prefixes []publicPrefix) publicPathVerdict {
	p := u.Path
	fixed := isFixedPublicPath(p)
	rawPrefix := "" // how the raw path must start, when a declared prefix matched
	if !fixed {
		matched := false
		for _, pre := range prefixes {
			if strings.HasPrefix(p, pre.decoded) {
				rawPrefix, matched = pre.rawPrefix, true
				break
			}
		}
		if !matched {
			return pathNotPublic
		}
	}
	if !isCanonicalPath(u) {
		return pathAmbiguous
	}
	// strings.HasPrefix(raw, "") is true: no raw check for a prefix with no raw spelling.
	if raw := u.EscapedPath(); (fixed && raw != p) || (!fixed && !strings.HasPrefix(raw, rawPrefix)) {
		return pathAmbiguous
	}
	return pathPublic
}

// isCanonicalPath reports whether u names its resource in exactly one way. All of
// these must hold:
//
//   - the decoded path is what path.Clean would make of it, except that it may end
//     in a slash (a prefix such as "/c/" needs that): no "." or ".." segment, no
//     empty segment ("//"), and it starts with "/";
//   - neither the decoded nor the raw form holds an encoded dot, slash or
//     backslash (%2e, %2f, %5c, in either case). In the raw form that is a
//     separator a router may or may not decode. In the decoded form it is a
//     double-encoded one: the request said %252e, and a router that decodes a
//     second time reads ".";
//   - the decoded path has no backslash, which some stacks read as a separator.
//
// With no encoded dot or slash in the raw form its segments are the decoded
// segments, so the dot-segment test on the decoded path covers the raw one too.
//
// A canonical path costs no allocation: the gate runs this on every request for a
// health probe.
func isCanonicalPath(u *url.URL) bool {
	p := u.Path
	if p == "" || p[0] != '/' {
		return false
	}
	if !isCleanPath(p) {
		return false
	}
	if strings.IndexByte(p, '\\') >= 0 || hasEncodedSeparator(p) {
		return false
	}
	return !hasEncodedSeparator(u.EscapedPath())
}

// isCleanPath reports whether p equals path.Clean(p), allowing one trailing slash.
func isCleanPath(p string) bool {
	c := path.Clean(p)
	if c == p {
		return true
	}
	// "/c/" cleans to "/c": canonical. "//" cleans to "/" and "/c//" to "/c": not,
	// so p minus its last slash must be the cleaned path itself, and not the root.
	return c != "/" && p[len(p)-1] == '/' && p[:len(p)-1] == c
}

// hasEncodedSeparator reports whether s holds %2e, %2f or %5c in any case.
func hasEncodedSeparator(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		switch hi, lo := s[i+1], s[i+2]|0x20; {
		case hi == '2' && (lo == 'e' || lo == 'f'):
			return true
		case hi == '5' && lo == 'c':
			return true
		}
	}
	return false
}

// denyBadPath refuses a request whose path names an exempt route in a form the
// gate will not vouch for. The gate does not pass the request on and does not
// look at its credentials.
//
// The status is 400, not 401. A 401 says "authenticate and try again", and no
// credential makes a path with dot segments or encoded separators unambiguous: a
// client that answered it by attaching a key would be sending the same path
// again, which the gate would refuse again, and the code that reads a 401 as "the
// key is bad or missing" (retry logic, key rotation, the missing-token and
// invalid-token counters on a dashboard) would be misled. The request target
// itself is what is wrong, so 400 is accurate and tells the caller what to fix: send
// the canonical path. It is also not a 404 or a redirect, which would let a
// prober tell which spellings reach a handler.
func denyBadPath(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Bad Request", "reason": "non-canonical path"})
}

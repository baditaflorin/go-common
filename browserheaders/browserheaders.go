// Package browserheaders generates a coherent set of browser-typical HTTP
// request headers for outbound calls that need to evade trivial bot detection.
//
// The fleet's canonical "I'm a bot, please rate-limit me" User-Agent
// (`ua.Build`) is appropriate for inbound APIs that want to be identified —
// but lethal for outbound calls to anti-bot endpoints like DuckDuckGo,
// Cloudflare-protected sites, etc.
//
// # Why "coherent set"
//
// Modern bot detectors (DDG, Cloudflare, Akamai, PerimeterX, DataDome)
// check that headers are internally consistent. Examples:
//   - Chrome's User-Agent → must come with Sec-Ch-Ua "Google Chrome"
//   - Firefox's User-Agent → must NOT send Sec-Ch-Ua
//   - Safari's User-Agent → must NOT send Sec-Ch-Ua
//   - Accept-Encoding should always include "br" on modern browsers
//   - Sending Referer together with Sec-Fetch-Site: none is contradictory;
//     use ApplyNavigation to keep Sec-Fetch-Site aligned with Referer.
//
// Mixing Chrome's UA with Firefox's Sec-Ch-Ua absence is a tell. So we
// rotate full personas (browser+OS+version), not individual fields.
//
// # Usage
//
//	persona := browserheaders.Random()
//	persona.Apply(req.Header)
//	// or with navigation Referer + consistent Sec-Fetch-Site:
//	persona.ApplyNavigation(req.Header, req.URL.String(), "https://duckduckgo.com/")
package browserheaders

import (
	"math/rand"
	"net/http"
	"net/url"
	"strings"
)

// Browser family constants so TLS-impersonating callers can match the TLS
// ClientHello (e.g. Chrome/Firefox/Safari) to the chosen HTTP persona.
const (
	FamilyChrome  = "chrome"
	FamilyFirefox = "firefox"
	FamilySafari  = "safari"
)

// Persona is one self-consistent browser identity.
type Persona struct {
	Family          string // FamilyChrome, FamilyFirefox, or FamilySafari
	UserAgent       string
	Accept          string
	AcceptLanguage  string
	AcceptEncoding  string
	SecChUa         string // empty if not a Chromium-family browser
	SecChUaMobile   string // ?0 (desktop) or ?1 (mobile)
	SecChUaPlatform string // "macOS" / "Windows" / "Linux" / etc.
	// Set Upgrade-Insecure-Requests: 1
	UpgradeInsecure bool
	// Set DNT: 1
	DoNotTrack bool
}

// Apply writes the persona's headers into h, replacing any existing values
// for these keys. Assumes a direct navigation (Sec-Fetch-Site: none).
func (p Persona) Apply(h http.Header) {
	p.ApplyNavigation(h, "", "")
}

// ApplyNavigation writes the persona's headers into h and sets Referer and
// Sec-Fetch-Site consistently with targetURL and referer:
//   - empty referer -> Sec-Fetch-Site: none (no Referer header)
//   - same scheme+host -> Sec-Fetch-Site: same-origin
//   - same registrable parent domain (e.g. duckduckgo.com -> html.duckduckgo.com) -> Sec-Fetch-Site: same-site
//   - different domain -> Sec-Fetch-Site: cross-site
func (p Persona) ApplyNavigation(h http.Header, targetURL, referer string) {
	h.Set("User-Agent", p.UserAgent)
	h.Set("Accept", p.Accept)
	h.Set("Accept-Language", p.AcceptLanguage)
	h.Set("Accept-Encoding", p.AcceptEncoding)
	if p.SecChUa != "" {
		h.Set("Sec-Ch-Ua", p.SecChUa)
		h.Set("Sec-Ch-Ua-Mobile", p.SecChUaMobile)
		h.Set("Sec-Ch-Ua-Platform", p.SecChUaPlatform)
	} else {
		h.Del("Sec-Ch-Ua")
		h.Del("Sec-Ch-Ua-Mobile")
		h.Del("Sec-Ch-Ua-Platform")
	}
	if p.UpgradeInsecure {
		h.Set("Upgrade-Insecure-Requests", "1")
	}
	if p.DoNotTrack {
		h.Set("DNT", "1")
	}
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", secFetchSiteFor(targetURL, referer))
	h.Set("Sec-Fetch-User", "?1")
	if strings.TrimSpace(referer) != "" {
		h.Set("Referer", referer)
	} else {
		h.Del("Referer")
	}
}

func secFetchSiteFor(targetURL, referer string) string {
	referer = strings.TrimSpace(referer)
	if referer == "" {
		return "none"
	}
	refU, err1 := url.Parse(referer)
	tgtU, err2 := url.Parse(strings.TrimSpace(targetURL))
	if err1 != nil || err2 != nil || refU.Hostname() == "" || tgtU.Hostname() == "" {
		return "cross-site"
	}
	refHost := strings.ToLower(refU.Hostname())
	tgtHost := strings.ToLower(tgtU.Hostname())
	if strings.EqualFold(refU.Scheme, tgtU.Scheme) && refHost == tgtHost && refU.Port() == tgtU.Port() {
		return "same-origin"
	}
	if strings.EqualFold(refU.Scheme, tgtU.Scheme) && shareParentDomain(refHost, tgtHost) {
		return "same-site"
	}
	return "cross-site"
}

func shareParentDomain(a, b string) bool {
	if a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a) {
		return true
	}
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	if len(pa) >= 2 && len(pb) >= 2 {
		return pa[len(pa)-1] == pb[len(pb)-1] && pa[len(pa)-2] == pb[len(pb)-2]
	}
	return false
}

// personas is the rotation pool. Each entry MUST be internally consistent:
// Chrome/Edge UAs get Sec-Ch-Ua headers; Firefox/Safari MUST NOT.
var personas = []Persona{
	// --- Chrome 136 / macOS ---
	{
		Family:          FamilyChrome,
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Chromium";v="136", "Google Chrome";v="136", "Not.A/Brand";v="99"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"macOS"`,
		UpgradeInsecure: true,
	},
	// --- Chrome 136 / Windows ---
	{
		Family:          FamilyChrome,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Chromium";v="136", "Google Chrome";v="136", "Not.A/Brand";v="99"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Windows"`,
		UpgradeInsecure: true,
	},
	// --- Chrome 135 / Linux ---
	{
		Family:          FamilyChrome,
		UserAgent:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Google Chrome";v="135", "Not-A.Brand";v="8", "Chromium";v="135"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Linux"`,
		UpgradeInsecure: true,
	},
	// --- Edge 136 / Windows (Chromium-based) ---
	{
		Family:          FamilyChrome,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36 Edg/136.0.0.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Chromium";v="136", "Microsoft Edge";v="136", "Not.A/Brand";v="99"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Windows"`,
		UpgradeInsecure: true,
	},
	// --- Firefox 138 / macOS (no Sec-Ch-Ua) ---
	{
		Family:          FamilyFirefox,
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:138.0) Gecko/20100101 Firefox/138.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Firefox 138 / Windows (no Sec-Ch-Ua) ---
	{
		Family:          FamilyFirefox,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:138.0) Gecko/20100101 Firefox/138.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Firefox 137 / Linux ---
	{
		Family:          FamilyFirefox,
		UserAgent:       "Mozilla/5.0 (X11; Linux x86_64; rv:137.0) Gecko/20100101 Firefox/137.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Safari 18.4 / macOS (no Sec-Ch-Ua) ---
	{
		Family:          FamilySafari,
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.4 Safari/605.1.15",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Safari 18.3 / macOS ---
	{
		Family:          FamilySafari,
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Chrome 135 / Android (mobile signal) ---
	{
		Family:          FamilyChrome,
		UserAgent:       "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Mobile Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Google Chrome";v="135", "Not-A.Brand";v="8", "Chromium";v="135"`,
		SecChUaMobile:   "?1",
		SecChUaPlatform: `"Android"`,
		UpgradeInsecure: true,
	},
}

// Random returns a random Persona from the pool.
// Safe for concurrent use; uses the default math/rand source.
func Random() Persona {
	//nolint:gosec // non-crypto random is fine for header rotation
	return personas[rand.Intn(len(personas))]
}

// ApplyRandom is shorthand for Random().Apply(h).
func ApplyRandom(h http.Header) {
	Random().Apply(h)
}

// Personas returns a copy of the internal persona list. Useful for tests
// and for callers that want to filter (e.g. desktop-only).
func Personas() []Persona {
	out := make([]Persona, len(personas))
	copy(out, personas)
	return out
}

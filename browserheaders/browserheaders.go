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
//
// Mixing Chrome's UA with Firefox's Sec-Ch-Ua absence is a tell. So we
// rotate full personas (browser+OS+version), not individual fields.
//
// # Usage
//
//	persona := browserheaders.Random()
//	persona.Apply(req.Header)
//	// or one-shot:
//	browserheaders.ApplyRandom(req.Header)
package browserheaders

import (
	"math/rand"
	"net/http"
)

// Persona is one self-consistent browser identity.
type Persona struct {
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
// for these keys.
func (p Persona) Apply(h http.Header) {
	h.Set("User-Agent", p.UserAgent)
	h.Set("Accept", p.Accept)
	h.Set("Accept-Language", p.AcceptLanguage)
	h.Set("Accept-Encoding", p.AcceptEncoding)
	if p.SecChUa != "" {
		h.Set("Sec-Ch-Ua", p.SecChUa)
		h.Set("Sec-Ch-Ua-Mobile", p.SecChUaMobile)
		h.Set("Sec-Ch-Ua-Platform", p.SecChUaPlatform)
	}
	if p.UpgradeInsecure {
		h.Set("Upgrade-Insecure-Requests", "1")
	}
	if p.DoNotTrack {
		h.Set("DNT", "1")
	}
	// Common to navigations from address-bar / link clicks.
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
}

// personas is the rotation pool. Each entry MUST be internally consistent:
// Chrome UAs get Sec-Ch-Ua headers; Firefox/Safari MUST NOT.
//
// Versions kept reasonably current — refresh annually to avoid looking
// suspiciously dated.
var personas = []Persona{
	// --- Chrome 121 / macOS ---
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Not A(Brand";v="99", "Google Chrome";v="121", "Chromium";v="121"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"macOS"`,
		UpgradeInsecure: true,
	},
	// --- Chrome 122 / Windows ---
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Chromium";v="122", "Not(A:Brand";v="24", "Google Chrome";v="122"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Windows"`,
		UpgradeInsecure: true,
	},
	// --- Chrome 121 / Linux ---
	{
		UserAgent:       "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Not A(Brand";v="99", "Google Chrome";v="121", "Chromium";v="121"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Linux"`,
		UpgradeInsecure: true,
	},
	// --- Edge 121 / Windows (Chromium-based) ---
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36 Edg/121.0.0.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Not A(Brand";v="99", "Microsoft Edge";v="121", "Chromium";v="121"`,
		SecChUaMobile:   "?0",
		SecChUaPlatform: `"Windows"`,
		UpgradeInsecure: true,
	},
	// --- Firefox 122 / macOS (no Sec-Ch-Ua) ---
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 14.3; rv:122.0) Gecko/20100101 Firefox/122.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
		DoNotTrack:      true,
	},
	// --- Firefox 122 / Windows (no Sec-Ch-Ua) ---
	{
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:122.0) Gecko/20100101 Firefox/122.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Firefox 121 / Linux ---
	{
		UserAgent:       "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.5",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Safari 17 / macOS (no Sec-Ch-Ua) ---
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2.1 Safari/605.1.15",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Safari 16 / macOS ---
	{
		UserAgent:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Safari/605.1.15",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		UpgradeInsecure: true,
	},
	// --- Chrome 120 / Android (mobile signal) ---
	{
		UserAgent:       "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
		Accept:          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
		SecChUa:         `"Not A(Brand";v="99", "Google Chrome";v="120", "Chromium";v="120"`,
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

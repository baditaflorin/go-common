package botwall

import (
	"sort"
	"strings"
)

const (
	// MaxBody is the largest document, in bytes, that can be a challenge page.
	MaxBody = 160 << 10
	// MaxVisibleText is the most visible text (non-space bytes outside tags and
	// outside the bodies of script and style elements) a challenge page has.
	MaxVisibleText = 3000
)

// Vendors are the values [Detect] can return. A service that counts walls per
// vendor lists all of them from the start so rate() and absent() alerts see
// every label. The slice is sorted; the caller gets a copy.
func Vendors() []string {
	out := []string{"akamai", "aws_waf", "cloudflare", "datadome", "google_sorry", "imperva", "perimeterx", "sucuri"}
	sort.Strings(out)
	return out
}

// Detect returns the vendor of a challenge page, or "" when the document is not
// one.
func Detect(html string) string {
	if html == "" || len(html) > MaxBody {
		return ""
	}
	l := strings.ToLower(html)
	vis := visibleTextLen(l)
	if vis > MaxVisibleText {
		return ""
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if !strings.Contains(l, s) {
				return false
			}
		}
		return true
	}
	switch {
	case has("captcha-delivery.com", "var dd="):
		return "datadome"
	case cloudflareChallenge(l):
		return "cloudflare"
	case strings.Contains(l, "px-captcha") || strings.Contains(l, "captcha.px-cdn.net"):
		return "perimeterx"
	case has("<title>access denied</title>", "edgesuite"):
		// The reference URL is HTML-entity encoded in the page (errors&#46;edgesuite&#46;net), so the host is
		// matched by its first label only.
		return "akamai"
	case impervaChallenge(l, vis):
		return "imperva"
	case awsWAFChallenge(l):
		return "aws_waf"
	case has("sucuri website firewall", "access denied"):
		return "sucuri"
	case strings.Contains(l, "our systems have detected unusual traffic from your computer network"):
		return "google_sorry"
	}
	return ""
}

// impervaChallenge: the block page ("Request unsuccessful. Incapsula incident ID: ..." inside the iframe fallback) or the
// JavaScript interstitial, which is an empty body around one /_Incapsula_Resource script. The script reference alone is NOT
// enough: Imperva injects it into every HTML response of a protected site, so a real page, a real 404 page included, carries it.
// (2026-10-18, found on live traffic: real 404 pages of abbeyroad.com and abelandcole.co.uk, 28-33 KB with 1.1-1.7 KB of visible
// text, were flagged by the script reference alone.)
func impervaChallenge(l string, visible int) bool {
	if strings.Contains(l, "incapsula incident id") {
		return true
	}
	return strings.Contains(l, "_incapsula_resource") && visible <= 10
}

// awsWAFChallenge: the interstitial's own snippet, AwsWafIntegration.saveReferrer() then .checkForceRefresh(), or its noscript
// notice. The AwsWafIntegration object, challenge.js and getToken() are also used by ordinary pages that integrate the SDK
// (a16zcrypto.com's real 404 page was flagged by the object name alone), so none of those is enough.
func awsWAFChallenge(l string) bool {
	if strings.Contains(l, "awswafintegration.checkforcerefresh") {
		return true
	}
	return strings.Contains(l, "awswafintegration") && strings.Contains(l, "verify that you") && strings.Contains(l, "robot")
}

// cloudflareChallenge: the "Just a moment..." interstitial or the "Attention Required" block page.
// The jsd script (/cdn-cgi/challenge-platform/scripts/jsd/main.js) that Cloudflare adds to ordinary
// pages is NOT enough on its own.
func cloudflareChallenge(l string) bool {
	if strings.Contains(l, "attention required! | cloudflare") {
		return true
	}
	if strings.Contains(l, "sorry, you have been blocked") && strings.Contains(l, "cloudflare") {
		return true
	}
	interstitial := strings.Contains(l, "<title>just a moment") ||
		strings.Contains(l, "checking your browser before accessing") ||
		strings.Contains(l, "cf_chl_opt") ||
		strings.Contains(l, "cf-browser-verification") ||
		strings.Contains(l, "enable javascript and cookies to continue")
	return interstitial && (strings.Contains(l, "cdn-cgi/challenge-platform") ||
		strings.Contains(l, "challenges.cloudflare.com") ||
		strings.Contains(l, "cf_chl_opt") ||
		strings.Contains(l, "cloudflare"))
}

// visibleTextLen counts the non-space bytes of s outside tags and outside the bodies of script and
// style elements. s must already be lower-cased. It is a size heuristic, not an HTML parser: an
// unterminated script counts as the rest of the document, a stray '<' in text skips to the next '>'.
func visibleTextLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] != '<' {
			if s[i] > ' ' {
				n++
			}
			i++
			continue
		}
		for _, raw := range [...]string{"script", "style"} {
			if hasTagPrefix(s[i:], raw) {
				end := strings.Index(s[i:], "</"+raw)
				if end < 0 {
					return n
				}
				i += end
				break
			}
		}
		gt := strings.IndexByte(s[i:], '>')
		if gt < 0 {
			return n
		}
		i += gt + 1
	}
	return n
}

// hasTagPrefix reports whether s starts with the opening tag <name (followed by a space, '/' or '>').
func hasTagPrefix(s, name string) bool {
	if len(s) <= 1+len(name) || s[1:1+len(name)] != name {
		return false
	}
	switch s[1+len(name)] {
	case ' ', '\t', '\n', '\r', '/', '>':
		return true
	}
	return false
}

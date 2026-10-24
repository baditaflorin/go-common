package botwall

import (
	"sort"
	"strings"
	"testing"
)

// Fixtures: the structure of each vendor's interstitial, trimmed (no session tokens). The DataDome one is the
// page nytimes.com and reuters.com returned through the egress proxy on 2026-10-18 (1,486 bytes).
const (
	fixDataDome = `<html lang="en"><head><title>nytimes.com</title><style>#cmsg{animation: A 1.5s;}@keyframes A{0%{opacity:0;}99%{opacity:0;}100%{opacity:1;}}</style><meta name="viewport" content="width=device-width, initial-scale=1.0"></head><body style="margin:0"><script data-cfasync="false">var dd={'rt':'i','cid':'AHrl','hsh':'499A','b':1979720,'s':60446,'e':'b56c','qp':'','host':'geo.captcha-delivery.com','cookie':'tqt9'}</script><script data-cfasync="false" src="https://ct.captcha-delivery.com/i.js"></script></body></html>`

	fixCloudflareWait = `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title><meta http-equiv="refresh" content="390"></head><body><div class="main-wrapper"><h1>example.com</h1><h2>Checking your browser before accessing</h2><noscript>Enable JavaScript and cookies to continue</noscript></div><script>window._cf_chl_opt={cvId:'3'};</script><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"></script></body></html>`

	fixCloudflareBlock = `<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body><h1>Sorry, you have been blocked</h1><p>You are unable to access example.com</p><span class="cf-error-details">Cloudflare Ray ID: 8a</span></body></html>`

	fixPerimeterX = `<html><head><title>Access to this page has been denied</title></head><body><div id="px-captcha"></div><p>Press &amp; Hold to confirm you are a human</p><script src="https://captcha.px-cdn.net/PXabc/captcha.js"></script></body></html>`

	fixAkamai = `<HTML><HEAD><TITLE>Access Denied</TITLE></HEAD><BODY><H1>Access Denied</H1> You don't have permission to access "http&#58;&#47;&#47;www&#46;example&#46;com&#47;" on this server.<P>Reference&#32;&#35;18&#46;abc&#46;123<P>https&#58;&#47;&#47;errors&#46;edgesuite&#46;net&#47;18&#46;abc&#46;123</BODY></HTML>`

	fixImperva = `<html><head><META NAME="robots" CONTENT="noindex,nofollow"></head><body><iframe src="/_Incapsula_Resource?SWUDNSAI=31&xinfo=1" frameborder=0 width="100%" height="100%">Request unsuccessful. Incapsula incident ID: 1-2</iframe></body></html>`

	fixAWSWAF = `<html><head><title></title></head><body><script src="https://abc.token.awswaf.com/abc/challenge.js"></script><script>AwsWafIntegration.saveReferrer();AwsWafIntegration.checkForceRefresh().then(function(){});</script></body></html>`

	fixSucuri = `<html><head><title>Sucuri WebSite Firewall - Access Denied</title></head><body><h1>Access Denied - Sucuri Website Firewall</h1><p>If you are the site owner, please contact support.</p></body></html>`

	fixGoogleSorry = `<html><body><div>Our systems have detected unusual traffic from your computer network. This page checks to see if it's really you sending the requests, and not a robot.</div></body></html>`
)

func TestDetect_Vendors(t *testing.T) {
	for _, c := range []struct{ name, html, want string }{
		{"datadome", fixDataDome, "datadome"},
		{"cloudflare wait", fixCloudflareWait, "cloudflare"},
		{"cloudflare block", fixCloudflareBlock, "cloudflare"},
		{"perimeterx", fixPerimeterX, "perimeterx"},
		{"akamai", fixAkamai, "akamai"},
		{"imperva", fixImperva, "imperva"},
		{"aws waf", fixAWSWAF, "aws_waf"},
		{"sucuri", fixSucuri, "sucuri"},
		{"google sorry", fixGoogleSorry, "google_sorry"},
	} {
		if got := Detect(c.html); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every vendor Detect can return is listed in Vendors (the label set a service pre-registers), and the list has nothing
// Detect cannot return: each fixture above maps to one entry and each entry has a fixture.
func TestVendorsAreExactlyWhatDetectReturns(t *testing.T) {
	fixtures := map[string]string{
		"datadome": fixDataDome, "cloudflare": fixCloudflareWait, "perimeterx": fixPerimeterX, "akamai": fixAkamai,
		"imperva": fixImperva, "aws_waf": fixAWSWAF, "sucuri": fixSucuri, "google_sorry": fixGoogleSorry,
	}
	vendors := Vendors()
	if len(vendors) != len(fixtures) {
		t.Fatalf("Vendors() = %v (%d), want exactly the %d vendors with a fixture", vendors, len(vendors), len(fixtures))
	}
	for _, v := range vendors {
		html, ok := fixtures[v]
		if !ok {
			t.Errorf("vendor %q is listed but has no fixture here", v)
			continue
		}
		if got := Detect(html); got != v {
			t.Errorf("fixture of %q detects as %q", v, got)
		}
	}
}

func TestVendorsIsSortedAndACopy(t *testing.T) {
	v := Vendors()
	if !sort.StringsAreSorted(v) {
		t.Errorf("Vendors() = %v, want sorted", v)
	}
	v[0] = "mutated"
	if Vendors()[0] == "mutated" {
		t.Error("Vendors() returned its backing array: a caller's edit leaked into the next call")
	}
}

func TestDetect_OrdinaryPagesAreNotFlagged(t *testing.T) {
	prose := strings.Repeat("A paragraph of real article text about something. ", 120) // > 3,000 bytes of visible text
	for _, c := range []struct{ name, html string }{
		{"empty", ""},
		{"plain page", `<html><head><title>Hello</title></head><body><h1>Hello</h1><p>World.</p></body></html>`},
		{"example.com", `<html><head><title>Example Domain</title></head><body><div><h1>Example Domain</h1><p>This domain is for use in illustrative examples in documents.</p></div></body></html>`},
		// Cloudflare adds its jsd script to ordinary pages: the script alone is not an interstitial
		{"cloudflare jsd script on a short real page", `<html><head><title>My blog</title></head><body><h1>My blog</h1><p>Text.</p><script>(function(){var s=document.createElement('script');s.src='/cdn-cgi/challenge-platform/scripts/jsd/main.js';document.head.appendChild(s)})()</script></body></html>`},
		// a site that only embeds the DataDome tag: no challenge object, and real text
		{"datadome tag on a real page", `<html><head><script>window.ddjskey='ABC';window.ddoptions={endpoint:'https://api-js.datadome.co/js/'}</script><script src="https://js.datadome.co/tags.js" async></script></head><body>` + prose + `</body></html>`},
		// the same markers inside a text-heavy page (a blog post about bot protection)
		{"post about px-captcha", `<html><body><article><p>The PerimeterX challenge uses an element with id px-captcha.</p>` + prose + `</article></body></html>`},
		{"post about datadome internals", `<html><body><p>The page defines var dd= and loads captcha-delivery.com</p>` + prose + `</body></html>`},
		// each vendor rule needs the vendor's own marker pair, not one generic half of it
		{"datadome host without the challenge object", `<html><body><script src="https://ct.captcha-delivery.com/i.js"></script></body></html>`},
		{"generic Access Denied page, no vendor", `<html><head><title>Access Denied</title></head><body><h1>Access Denied</h1><p>You do not have permission to view this page.</p></body></html>`},
		{"sucuri name without the denial", `<html><body><p>Protected by Sucuri Website Firewall.</p></body></html>`},
		// big documents are never challenge pages, whatever they contain
		{"over the size cap", fixDataDome + strings.Repeat(" ", MaxBody)},
	} {
		if got := Detect(c.html); got != "" {
			t.Errorf("%s: flagged as %q, want not a challenge", c.name, got)
		}
	}
}

// The size cap is exact: a wall at MaxBody bytes is still seen, one byte more is not.
func TestDetect_SizeCapBoundary(t *testing.T) {
	pad := func(n int) string { return fixDataDome + strings.Repeat(" ", n-len(fixDataDome)) }
	if got := Detect(pad(MaxBody)); got != "datadome" {
		t.Errorf("a wall of exactly MaxBody bytes: Detect = %q, want datadome", got)
	}
	if got := Detect(pad(MaxBody + 1)); got != "" {
		t.Errorf("a wall of MaxBody+1 bytes: Detect = %q, want not a challenge", got)
	}
}

// The visible-text cap is exact too: MaxVisibleText non-space bytes still count as a wall, one more does not.
func TestDetect_VisibleTextBoundary(t *testing.T) {
	mk := func(n int) string {
		return `<html><body><div id="px-captcha"></div><p>` + strings.Repeat("x", n) + `</p></body></html>`
	}
	base := visibleTextLen(strings.ToLower(mk(0)))
	if got := Detect(mk(MaxVisibleText - base)); got != "perimeterx" {
		t.Errorf("a wall with exactly MaxVisibleText visible bytes: Detect = %q, want perimeterx", got)
	}
	if got := Detect(mk(MaxVisibleText - base + 1)); got != "" {
		t.Errorf("a wall with MaxVisibleText+1 visible bytes: Detect = %q, want not a challenge", got)
	}
}

func TestVisibleTextLen(t *testing.T) {
	for _, c := range []struct {
		html string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"a b\n c", 3},
		{"<p>abc</p>", 3},
		{"<script>var x = 12345;</script>ab", 2},
		{"<style>p{color:red}</style><p>x</p>", 1},
		{"<SCRIPT src=x></SCRIPT>y", 1}, // callers lower-case first; uppercase tags are treated as text-free tags
		{"<script>unterminated", 0},
		{"text<unterminated", 4},
		{"<scripty>abc</scripty>", 3}, // not a script element
	} {
		if got := visibleTextLen(strings.ToLower(c.html)); got != c.want {
			t.Errorf("visibleTextLen(%q) = %d, want %d", c.html, got, c.want)
		}
	}
}

// The vendor's injected script is on ordinary pages too. Real pages found flagged on live traffic (2026-10-18): 404 pages of
// Imperva- and AWS-WAF-fronted sites, 28-46 KB with 1-2 KB of visible text.
func TestDetect_VendorScriptOnARealPageIsNotAWall(t *testing.T) {
	text := strings.Repeat("Page not found. The page you are looking for does not exist, go back to the home page. ", 20) // ~1.4 KB visible
	for _, c := range []struct{ name, html string }{
		{"imperva script on a real 404 page", `<!DOCTYPE html><html lang="en"><head><title>Page not found</title></head><body><nav><a href="/">Home</a><a href="/about">About</a></nav><h1>Page not found</h1><p>` + text + `</p><script type="text/javascript" src="/_Incapsula_Resource?SWJIYLWA=719d34d31c8e3a6e6fffd425f7e032f3"></script></body></html>`},
		{"aws waf sdk on a real 404 page", `<!DOCTYPE html><html lang="en"><head><title>404</title><script src="https://abc.edge.sdk.awswaf.com/abc/def/challenge.js" defer></script></head><body><h1>404</h1><p>` + text + `</p><script>AwsWafIntegration.getToken().then(function(){});</script></body></html>`},
		{"aws waf captcha integration on a real page", `<html><head><title>Sign in</title></head><body><h1>Sign in</h1><p>` + text + `</p><div id="captcha"></div><script>AwsWafCaptcha.renderCaptcha(document.getElementById("captcha"),{});var x=window.AwsWafIntegration;</script></body></html>`},
	} {
		if got := Detect(c.html); got != "" {
			t.Errorf("%s: flagged as %q, want not a challenge", c.name, got)
		}
	}
	// and the real interstitials still are
	for _, c := range []struct{ name, html, want string }{
		{"imperva js interstitial (empty body around one script)", `<html><head><META NAME="robots" CONTENT="noindex,nofollow"><script src="/_Incapsula_Resource?SWJIYLWA=719d34d31c8e3a6e6fffd425f7e032f3"></script></head><body></body></html>`, "imperva"},
		{"imperva block page", fixImperva, "imperva"},
		{"aws waf interstitial", fixAWSWAF, "aws_waf"},
		{"aws waf interstitial, noscript notice", `<html><head><noscript><h1>JavaScript is disabled</h1>In order to continue, we need to verify that you're not a robot. This requires JavaScript.</noscript></head><body><script>var w=AwsWafIntegration;</script></body></html>`, "aws_waf"},
	} {
		if got := Detect(c.html); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", c.name, got, c.want)
		}
	}
}

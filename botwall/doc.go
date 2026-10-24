// Package botwall recognises the interstitial pages a bot-protection vendor
// answers a client it distrusts with: a captcha, a "checking your browser"
// wait, an "Access Denied" page.
//
// A fetcher that stores or analyses whatever HTML came back keeps such a page
// as the site's content (2026-10-18, 100 real sites through the egress proxy:
// nytimes.com and reuters.com came back as one DataDome page of 1,486 bytes).
// [Detect] says which vendor's wall a document is, or that it is none, so the
// service can flag it, refuse it, or leave it alone.
//
// The detector is deliberately conservative, because a false positive discards
// real content when a caller refuses walls:
//
//   - only small documents ([MaxBody]: a challenge page is a few KB; a real page
//     that merely embeds a vendor's script is not),
//   - only documents with little visible text ([MaxVisibleText] non-space bytes
//     outside tags, script and style),
//   - and only vendor-specific markers, never a generic "captcha" or "access
//     denied".
//
// Not covered, by design: site-specific error pages (eBay's "Error Page"),
// walls that render something that looks like content, and any vendor not in
// [Vendors].
//
// It was written twice, identically, in go-js-proxy (0.12.0, 0.12.2) and the
// fleet fetch cache (0.3.48, 0.3.49); this package is the one copy, so a new
// vendor rule or a precision fix is made once.
package botwall

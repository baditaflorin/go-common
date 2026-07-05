// Package nullify is the canonical "I checked and found nothing" helper
// for enrichers writing a verdict/classification column to Postgres.
//
// Observed live across a 2026-07-05 audit of the domain_* enrichment
// tables (go-url-categorizer-api/docs/audit/2026-07-05-enricher-quality-audit.md):
// at least six enrichers write a literal placeholder string
// ("unknown", "Unknown", "n/a", "none", "") into a text column instead
// of leaving it NULL when no real signal was found. That's not six
// unrelated bugs, it's the same missing call in six places — every
// downstream consumer (API filters, dashboards, TRL scoring) has to
// special-case "is this value real or a fossilized default" per
// column instead of trusting SQL NULL to mean "not determined."
//
// Use StringOrNil at the boundary where an enricher's Go struct is
// about to be marshaled/written: replace
//
//	row.Verdict = classify(evidence) // may return "unknown"
//
// with
//
//	row.Verdict = nullify.StringOrNil(classify(evidence))
//
// so the column round-trips as NULL, not the string "unknown".
//
// This package intentionally does NOT address the sibling bug where a
// numeric column writes a sentinel 0 for "not checked" (see
// domain_price_range in the audit above) — that's a struct-shape fix
// (use *int / sql.NullInt64 fields), not a value-transform one, and
// needs a per-service decision about what a genuine zero means.
package nullify

import "strings"

// placeholders is deliberately small and generic — it is the same set
// the go-url-categorizer-api enricher-quality-audit tool flags as
// HIGH_PLACEHOLDER_RATE. Add to it centrally rather than growing
// per-service placeholder lists.
var placeholders = map[string]struct{}{
	"":            {},
	"unknown":     {},
	"n/a":         {},
	"na":          {},
	"none":        {},
	"null":        {},
	"-":           {},
	"tbd":         {},
	"not found":   {},
	"not-found":   {},
	"unavailable": {},
}

// IsPlaceholder reports whether s (after trimming whitespace and
// lowercasing) is one of the known "no real signal" placeholder
// strings. Enrichers can use this to gate a write ("only persist this
// field if it's not a placeholder") without allocating.
func IsPlaceholder(s string) bool {
	_, ok := placeholders[strings.ToLower(strings.TrimSpace(s))]
	return ok
}

// StringOrNil returns nil when s is a known placeholder (per
// IsPlaceholder), otherwise returns a pointer to the trimmed string.
// Assign the result directly to a `*string` struct field that a
// jsonb/text column marshals from.
func StringOrNil(s string) *string {
	trimmed := strings.TrimSpace(s)
	if IsPlaceholder(trimmed) {
		return nil
	}
	return &trimmed
}

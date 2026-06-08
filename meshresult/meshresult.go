// Package meshresult is the fleet-wide canonical classification of an
// enricher's result, plus the HTTP-status mapping its consumer
// (domainscope) keys on.
//
// # Why this package exists
//
// ~70 enricher microservices on *.0crawl.com / *.0exec.com each fetch
// something for a domain (DNS, HTTP, TLS, an upstream API) and report
// back. Their consumer, domainscope, does NOT parse a service-specific
// body — it records each result purely by the enricher's HTTP STATUS
// CODE (see domainscope's servicemesh_status.go):
//
//	enricher HTTP 2xx        -> ServiceStatusOK       (1) "has data"
//	enricher HTTP 404 or 410 -> ServiceStatusNoData   (4) "reached, no data"
//	enricher HTTP 5xx / net  -> ServiceStatusUpstream (2) "upstream error"
//	context deadline (504)   -> ServiceStatusTimeout  (3) "timed out"
//
// Across four hardening rounds we found every enricher hand-rolling the
// same `httpStatusFor` logic: classify a fetch/DNS error into
// ok/no_data/unreachable/error, map that to an HTTP code (so domainscope
// records the truth — an unreachable host must be 404 NoData, NOT a false
// 200 OK), and emit a machine-readable body field. This package collapses
// that per-repo boilerplate into one tested place.
//
// # Three pieces
//
//   - Outcome — the canonical classification enum (ok / no_data /
//     unreachable / timeout / error).
//   - Outcome.HTTPCode — the status code domainscope keys on.
//   - ClassifyFetchError — turn a fetch/DNS/transport error into an
//     Outcome plus a stable, machine-readable reason token.
//   - WriteOutcome / OutcomeEnvelope — write (or build) a response that
//     carries the right HTTP code AND the classification in the body, on
//     top of the existing response envelope so the shape is fleet-uniform.
//
// # Body contract (stable — domainscope may read this later)
//
// domainscope keys on the HTTP code TODAY. The body additionally carries
// the finer classification under two STABLE top-level fields so a future
// domainscope upgrade can read the distinction the HTTP code flattens
// (no_data and unreachable both map to 404):
//
//	{
//	  "status": "success",                 // envelope status (unchanged)
//	  "result": "unreachable",             // <- meshresult.Outcome (STABLE)
//	  "reason": "dns_nxdomain",            // <- machine-readable token (STABLE)
//	  "data":   {...}                      // present only when result == "ok"
//	}
//
// The envelope's own `status` field stays "success"/"error" as every
// fleet client already expects; `result` is the orthogonal enricher-level
// classification. Read `result`, not `status`, for the enricher outcome.
//
// # What this package does NOT cover
//
// SSRF-block and bad-scheme are CALLER errors (the caller handed the
// enricher a bad/disallowed URL), not "the host is unreachable". They
// must surface as HTTP 400 and the handler deals with them separately —
// ClassifyFetchError returns OutcomeError/"bad_request" for the SSRF /
// bad-scheme markers so they never get laundered into a 404 NoData.
//
// This package imports only the fleet response package and the stdlib —
// it deliberately does NOT import safehttp/fleetfetch (that would create
// an import cycle and pull heavy deps into every enricher). Upstream
// error shapes are matched by documented string markers instead.
//
// # Example — an enricher handler (replaces per-repo httpStatusFor)
//
//	func handle(w http.ResponseWriter, r *http.Request) {
//	    rec, err := fetchAndEnrich(r.Context(), r.URL.Query().Get("domain"))
//	    if err != nil {
//	        outcome, reason := meshresult.ClassifyFetchError(err)
//	        meshresult.WriteOutcome(w, outcome, reason, nil)
//	        return
//	    }
//	    meshresult.WriteOutcome(w, meshresult.OutcomeOK, "", rec)
//	}
package meshresult

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/baditaflorin/go-common/response"
)

// Outcome is the canonical, fleet-wide classification of an enricher
// result. It is the single vocabulary every enricher uses instead of an
// ad-hoc per-repo enum.
type Outcome string

const (
	// OutcomeOK — the enricher reached the target and has data. HTTP 200.
	OutcomeOK Outcome = "ok"
	// OutcomeNoData — the enricher reached/looked but there is genuinely
	// nothing to report (e.g. domain has no MX records). HTTP 404.
	OutcomeNoData Outcome = "no_data"
	// OutcomeUnreachable — the target could not be reached at all
	// (NXDOMAIN, connection refused, TLS handshake failure). HTTP 404 —
	// domainscope records this as NoData(4) today; the body `result`
	// preserves the finer "unreachable" distinction. HTTP 404.
	OutcomeUnreachable Outcome = "unreachable"
	// OutcomeTimeout — the operation exceeded its deadline. HTTP 504.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeError — an upstream/transport/decode error that is neither a
	// clean "no data" nor a clean "unreachable", or a caller error. HTTP 502.
	OutcomeError Outcome = "error"
)

// HTTPCode returns the HTTP status code domainscope keys on for this
// Outcome. The mapping is intentionally the contract between every
// enricher and domainscope:
//
//	ok          -> 200  (ServiceStatusOK)
//	no_data     -> 404  (ServiceStatusNoData)
//	unreachable -> 404  (ServiceStatusNoData — flattened; see body `result`)
//	timeout     -> 504  (ServiceStatusTimeout)
//	error       -> 502  (ServiceStatusUpstreamError)
//
// An unknown Outcome conservatively maps to 502 so a typo never gets
// laundered into a false 200 OK.
func (o Outcome) HTTPCode() int {
	switch o {
	case OutcomeOK:
		return http.StatusOK // 200
	case OutcomeNoData, OutcomeUnreachable:
		return http.StatusNotFound // 404
	case OutcomeTimeout:
		return http.StatusGatewayTimeout // 504
	case OutcomeError:
		return http.StatusBadGateway // 502
	default:
		return http.StatusBadGateway // 502 — conservative
	}
}

// Stable reason tokens returned by ClassifyFetchError. These are part of
// the body contract; do not rename without a fleet-wide migration.
const (
	ReasonNone           = ""                // ok / no error
	ReasonDNSNXDomain    = "dns_nxdomain"    // host does not resolve
	ReasonDNSTimeout     = "dns_timeout"     // DNS lookup timed out
	ReasonDNSError       = "dns_error"       // other DNS failure
	ReasonConnectRefused = "connect_refused" // TCP connection refused
	ReasonConnectTimeout = "connect_timeout" // TCP connect timed out
	ReasonTLSError       = "tls_error"       // TLS handshake / cert failure
	ReasonFetchTimeout   = "fetch_timeout"   // context deadline / request timeout
	ReasonUpstream4xx    = "upstream_4xx"    // upstream returned a 4xx
	ReasonUpstream5xx    = "upstream_5xx"    // upstream returned a 5xx
	ReasonDecodeError    = "decode_error"    // response body failed to decode
	ReasonBadRequest     = "bad_request"     // caller error (SSRF block, bad scheme)
	ReasonUnknown        = "unknown_error"   // unclassifiable transport error
)

// ClassifyFetchError maps a fetch/DNS/transport error into an Outcome and
// a stable machine-readable reason token.
//
// It is implemented with stdlib only — errors.Is against
// context.DeadlineExceeded, *net.DNSError (IsNotFound / IsTimeout), the
// net.Error.Timeout() interface — plus documented string-marker matching
// for the fleetfetch / upstream-status shapes (which do not export typed
// sentinels). String matching is a deliberate trade-off to avoid an
// import cycle on safehttp/fleetfetch; the markers below are the
// documented contract.
//
// Caller errors (SSRF block, bad scheme) are NOT "unreachable" — they
// return (OutcomeError, ReasonBadRequest) and the handler is expected to
// surface them as HTTP 400, never as a 404 NoData.
//
// A nil error returns (OutcomeOK, ReasonNone).
func ClassifyFetchError(err error) (Outcome, string) {
	if err == nil {
		return OutcomeOK, ReasonNone
	}

	// 1. Context deadline — the canonical timeout signal.
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout, ReasonFetchTimeout
	}

	// 2. Typed DNS errors carry IsNotFound / IsTimeout.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return OutcomeUnreachable, ReasonDNSNXDomain
		case dnsErr.IsTimeout:
			return OutcomeTimeout, ReasonDNSTimeout
		default:
			return OutcomeUnreachable, ReasonDNSError
		}
	}

	// 3. Generic net.Error timeouts (dial/read deadline exceeded).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout, ReasonConnectTimeout
	}

	// 4. String-marker matching for shapes without typed sentinels
	//    (fleetfetch wraps with fmt.Errorf; upstream status codes are
	//    formatted into the message; OS connection errors stringify
	//    consistently). Lowercased once.
	msg := strings.ToLower(err.Error())

	switch {
	// Caller errors first — these MUST NOT be laundered into unreachable.
	case anyOf(msg, "blocked address", "non-public network", "ssrf"):
		return OutcomeError, ReasonBadRequest
	case anyOf(msg, "scheme must be", "invalid scheme", "missing host"):
		return OutcomeError, ReasonBadRequest

	// DNS.
	case anyOf(msg, "no such host", "name resolution", "nxdomain"):
		return OutcomeUnreachable, ReasonDNSNXDomain

	// TCP connect refused (check before the timeout marker so a refused
	// dial isn't mis-tagged as a timeout).
	case strings.Contains(msg, "connection refused"):
		return OutcomeUnreachable, ReasonConnectRefused

	// Deadline phrased as a string (wrapped, not the sentinel).
	case anyOf(msg, "context deadline exceeded", "request timeout", "timeout awaiting"):
		return OutcomeTimeout, ReasonFetchTimeout

	// TCP connect timeout (dial-level i/o timeout).
	case anyOf(msg, "i/o timeout", "connect: timed out", "operation timed out"):
		return OutcomeTimeout, ReasonConnectTimeout

	// TLS.
	case anyOf(msg, "tls:", "x509", "certificate", "handshake"):
		return OutcomeUnreachable, ReasonTLSError

	// Decode.
	case anyOf(msg, "decode", "unmarshal", "invalid character", "unexpected end of json"):
		return OutcomeError, ReasonDecodeError

	// Upstream status codes (fleetfetch / handler-formatted).
	case anyOf(msg, "500", "502", "503", "504", "5xx", "server error"):
		return OutcomeError, ReasonUpstream5xx
	case anyOf(msg, "404", "410", "not found", "gone"):
		return OutcomeNoData, ReasonUpstream4xx
	case anyOf(msg, "400", "401", "403", "4xx", "forbidden", "unauthorized"):
		return OutcomeError, ReasonUpstream4xx

	default:
		return OutcomeError, ReasonUnknown
	}
}

// anyOf reports whether s contains any one of the given substrings.
func anyOf(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// OutcomeEnvelope builds the response body for an outcome WITHOUT writing
// it, for handlers that need to control the http.ResponseWriter
// themselves (e.g. stamping extra headers or a schema version). It
// returns the HTTP code to use and the body map carrying the stable
// `result` / `reason` fields plus, on OutcomeOK, the data under `data`.
//
// The returned map reuses response.Success/NewError so the `status`
// field stays fleet-consistent ("success" for ok, "error" otherwise),
// then layers the orthogonal enricher-level `result`/`reason` on top.
func OutcomeEnvelope(outcome Outcome, reason string, data any) (code int, body map[string]any) {
	code = outcome.HTTPCode()

	var base response.Response
	if outcome == OutcomeOK {
		base = response.Success(data)
	} else {
		// error_code is the dotted machine-readable identifier; reuse the
		// reason token namespaced under "meshresult.".
		ec := "meshresult." + string(outcome)
		if reason != "" {
			ec = "meshresult." + reason
		}
		base = response.NewError(code, ec, string(outcome)+": "+reason)
	}

	// Marshal/unmarshal the envelope to a map so we can layer result/reason
	// on top without duplicating the envelope struct shape here.
	raw, _ := json.Marshal(base)
	body = map[string]any{}
	_ = json.Unmarshal(raw, &body)

	body["result"] = string(outcome)
	if reason != "" {
		body["reason"] = reason
	}
	return code, body
}

// WriteOutcome writes a complete fleet response for an enricher outcome:
// it sets the HTTP status to outcome.HTTPCode() and writes a JSON body
// carrying the stable `result` and `reason` classification fields (and,
// on OutcomeOK, the data under `data`).
//
// This is the canonical replacement for the per-repo `httpStatusFor` +
// hand-built body. domainscope keys on the HTTP status today and can read
// the body `result` field later for the finer no_data/unreachable split.
func WriteOutcome(w http.ResponseWriter, outcome Outcome, reason string, data any) {
	code, body := OutcomeEnvelope(outcome, reason, data)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

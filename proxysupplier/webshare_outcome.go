package proxysupplier

// Outcome classifies the result of one request made through a Webshare exit,
// for MarkOutcome and MarkSelectionOutcome. Unlike the bool of MarkResult, it
// tells the ASN and /24 health trackers which failures are evidence against
// the network (a bot challenge) and which are not (a dial error, a 5xx from
// the target), so that a target outage or a flaky route cannot quarantine an
// ASN.
//
// Every non-OK outcome still cools down the single exit IP exactly as
// MarkResult(addr, false) does; only the ASN and /24 accounting differs:
//
//	outcome           exit IP cooldown   ASN and /24 health
//	OutcomeOK         cleared            counted as ok
//	OutcomeChallenge  started            counted as a failure
//	OutcomeTransport  started            not counted
//	OutcomeUpstream   started            not counted
//
// The zero value and any value not listed are ignored (no state changes).
type Outcome uint8

const (
	// OutcomeOK is a usable answer.
	OutcomeOK Outcome = iota + 1
	// OutcomeChallenge is the target refusing this exit: a bot-challenge page,
	// HTTP 403 or HTTP 429. It is the only outcome that counts against the
	// exit's ASN and /24.
	OutcomeChallenge
	// OutcomeTransport is a failure to reach the target through the exit: a
	// dial, TLS, timeout or proxy (CONNECT) error.
	OutcomeTransport
	// OutcomeUpstream is the target failing on its own: an HTTP 5xx, or a
	// body that could not be decoded or parsed.
	OutcomeUpstream
)

// String returns the lower-case name used in logs ("ok", "challenge",
// "transport", "upstream"), or "unknown".
func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeChallenge:
		return "challenge"
	case OutcomeTransport:
		return "transport"
	case OutcomeUpstream:
		return "upstream"
	default:
		return "unknown"
	}
}

// OutcomeReporter is the outcome-kind form of ResultReporter, implemented by
// the webshare_direct and webshare_multi suppliers. addr is the same
// host:port key MarkResult takes.
type OutcomeReporter interface {
	MarkOutcome(addr string, outcome Outcome)
}

// WebshareSelectionOutcomeReporter is the outcome-kind form of
// WebshareSelectionReporter, implemented by the webshare_multi supplier.
type WebshareSelectionOutcomeReporter interface {
	MarkSelectionOutcome(selection WebshareSelection, outcome Outcome)
}

// healthVerdict is what one result contributes to the ASN and /24 trackers.
type healthVerdict uint8

const (
	healthNone healthVerdict = iota // not counted
	healthOK
	healthFail
)

// outcomeEffect maps an Outcome to its exit-IP verdict and its health verdict.
// valid is false for an unknown outcome, which callers ignore.
func outcomeEffect(o Outcome) (ipOK bool, verdict healthVerdict, valid bool) {
	switch o {
	case OutcomeOK:
		return true, healthOK, true
	case OutcomeChallenge:
		return false, healthFail, true
	case OutcomeTransport, OutcomeUpstream:
		return false, healthNone, true
	default:
		return false, healthNone, false
	}
}

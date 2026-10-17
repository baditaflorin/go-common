// Package meshclient is the HTTP client for calls to sibling fleet services on
// the private mesh: a base URL, a timeout, a bounded and strictly decoded JSON
// body, and one explicit answer to the question every such call has to settle:
// if the sibling is down, does the caller go on without it, or stop?
//
//	ledger, err := meshclient.New(meshclient.Config{
//	    Name:    "ledger",
//	    BaseURL: os.Getenv("LEDGER_SERVICE_URL"),
//	    Mode:    meshclient.FailOpen, // or FailClosed: there is no default
//	})
//	var out struct{ Balance int64 `json:"balance"` }
//	err = ledger.Do(ctx, meshclient.Request{Path: "/v1/balance"}, &out)
//	if meshclient.IsUnavailable(err) {
//	    // carry on without it; "ledger-down" is already in the request's degraded list
//	}
//
// # Why not safehttp
//
// safehttp is for the public internet and refuses private addresses by design, so
// a mesh call through it fails at once with ErrBlocked. Every service that needed
// a sibling therefore wrote its own net/http client, with its own timeout, body
// cap, decoding and failure handling. This is that client, written once.
//
// # Failing open or closed
//
// Mode is required. When the sibling is unavailable (the connection fails or times
// out, it answers 5xx or 429, its reply is unreadable, or the resilience policy
// refuses the call), Do returns an *UnavailableError, which satisfies
// errors.Is(err, ErrUnavailable). In FailOpen mode it also appends
// "<name>-down" (or -timeout, -rate_limited, -degraded) to the degraded.Sink in
// the context, so the response envelope tells the caller's client what is missing.
// In FailClosed mode it does not: the request is going to fail, there is nothing
// partial to report. A 4xx answer is never "unavailable": it is a *StatusError, the
// sibling's reply to a request that was wrong.
//
// Never use FailOpen for a check that protects something (credentials, charging,
// authorisation): carrying on without the answer would be the bypass.
//
// # What it does not do
//
// No redirects are followed. The request body is JSON. For retries, circuit
// breaking and load shedding pass a resilience.Policy in Config.
package meshclient

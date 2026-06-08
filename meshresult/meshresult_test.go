package meshresult

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
)

func TestOutcomeHTTPCode(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeOK, 200},
		{OutcomeNoData, 404},
		{OutcomeUnreachable, 404},
		{OutcomeTimeout, 504},
		{OutcomeError, 502},
		{Outcome("totally-unknown"), 502}, // conservative default
	}
	for _, tc := range cases {
		t.Run(string(tc.outcome), func(t *testing.T) {
			if got := tc.outcome.HTTPCode(); got != tc.want {
				t.Fatalf("HTTPCode(%q) = %d, want %d", tc.outcome, got, tc.want)
			}
		})
	}
}

// timeoutErr is a net.Error whose Timeout() reports true but which is not
// a *net.DNSError — exercises the generic net.Error branch.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "operation timed out" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassifyFetchError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantOut    Outcome
		wantReason string
	}{
		{"nil", nil, OutcomeOK, ReasonNone},

		// Typed branches.
		{"ctx_deadline", context.DeadlineExceeded, OutcomeTimeout, ReasonFetchTimeout},
		{"ctx_deadline_wrapped", fmt.Errorf("fetch: %w", context.DeadlineExceeded), OutcomeTimeout, ReasonFetchTimeout},
		{"dns_notfound", &net.DNSError{Err: "no such host", IsNotFound: true}, OutcomeUnreachable, ReasonDNSNXDomain},
		{"dns_timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, OutcomeTimeout, ReasonDNSTimeout},
		{"dns_other", &net.DNSError{Err: "server misbehaving"}, OutcomeUnreachable, ReasonDNSError},
		{"net_timeout", timeoutErr{}, OutcomeTimeout, ReasonConnectTimeout},

		// String-marker branches — caller errors must NOT become unreachable.
		{"ssrf_block", errors.New("blocked address: resolves to a non-public network"), OutcomeError, ReasonBadRequest},
		{"bad_scheme", errors.New("scheme must be http or https"), OutcomeError, ReasonBadRequest},
		{"missing_host", errors.New("missing host"), OutcomeError, ReasonBadRequest},

		// DNS via string.
		{"no_such_host_str", errors.New("dial tcp: lookup foo.example: no such host"), OutcomeUnreachable, ReasonDNSNXDomain},

		// TCP connect.
		{"refused", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), OutcomeUnreachable, ReasonConnectRefused},
		{"connect_timeout_str", errors.New("dial tcp 10.0.0.1:443: i/o timeout"), OutcomeTimeout, ReasonConnectTimeout},

		// TLS.
		{"tls_cert", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), OutcomeUnreachable, ReasonTLSError},

		// Deadline phrased as a string (not the sentinel).
		{"deadline_str", errors.New("Get \"https://x\": context deadline exceeded"), OutcomeTimeout, ReasonFetchTimeout},

		// Decode.
		{"decode", errors.New("json: cannot unmarshal number into Go value"), OutcomeError, ReasonDecodeError},
		{"bad_json", errors.New("unexpected end of json input"), OutcomeError, ReasonDecodeError},

		// Upstream status.
		{"upstream_5xx", errors.New("fleetfetch: cache status 502"), OutcomeError, ReasonUpstream5xx},
		{"upstream_503", errors.New("upstream returned 503 server error"), OutcomeError, ReasonUpstream5xx},
		{"upstream_404", errors.New("upstream returned status 404 not found"), OutcomeNoData, ReasonUpstream4xx},
		{"upstream_410", errors.New("status 410 gone"), OutcomeNoData, ReasonUpstream4xx},
		{"upstream_403", errors.New("upstream returned 403 forbidden"), OutcomeError, ReasonUpstream4xx},

		// Fallthrough.
		{"unknown", errors.New("some weird transport glitch"), OutcomeError, ReasonUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotReason := ClassifyFetchError(tc.err)
			if gotOut != tc.wantOut || gotReason != tc.wantReason {
				t.Fatalf("ClassifyFetchError(%v) = (%q, %q), want (%q, %q)",
					tc.err, gotOut, gotReason, tc.wantOut, tc.wantReason)
			}
		})
	}
}

// Every reason token a classifier can emit must map to a sensible code —
// guards against adding a token without wiring HTTPCode.
func TestReasonTokensClassifyToValidCode(t *testing.T) {
	// (outcome) pairs that are reachable from ClassifyFetchError.
	for _, o := range []Outcome{OutcomeOK, OutcomeNoData, OutcomeUnreachable, OutcomeTimeout, OutcomeError} {
		if code := o.HTTPCode(); code < 200 || code >= 600 {
			t.Fatalf("outcome %q -> invalid http code %d", o, code)
		}
	}
}

func TestWriteOutcome(t *testing.T) {
	cases := []struct {
		name       string
		outcome    Outcome
		reason     string
		data       any
		wantCode   int
		wantStatus string // envelope status
		wantData   bool   // data key present?
	}{
		{"ok", OutcomeOK, "", map[string]any{"mx": []string{"a"}}, 200, "success", true},
		{"no_data", OutcomeNoData, ReasonUpstream4xx, nil, 404, "error", false},
		{"unreachable", OutcomeUnreachable, ReasonDNSNXDomain, nil, 404, "error", false},
		{"timeout", OutcomeTimeout, ReasonFetchTimeout, nil, 504, "error", false},
		{"error", OutcomeError, ReasonUpstream5xx, nil, 502, "error", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteOutcome(rec, tc.outcome, tc.reason, tc.data)

			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("content-type = %q", ct)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not json: %v\n%s", err, rec.Body.String())
			}

			if got := body["result"]; got != string(tc.outcome) {
				t.Fatalf("body result = %v, want %q", got, tc.outcome)
			}
			if tc.reason != "" {
				if got := body["reason"]; got != tc.reason {
					t.Fatalf("body reason = %v, want %q", got, tc.reason)
				}
			}
			if got := body["status"]; got != tc.wantStatus {
				t.Fatalf("body status = %v, want %q", got, tc.wantStatus)
			}
			if _, hasData := body["data"]; hasData != tc.wantData {
				t.Fatalf("body data present = %v, want %v (body=%s)", hasData, tc.wantData, rec.Body.String())
			}
		})
	}
}

func TestOutcomeEnvelopeNoWrite(t *testing.T) {
	code, body := OutcomeEnvelope(OutcomeOK, "", map[string]any{"k": "v"})
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if body["result"] != "ok" {
		t.Fatalf("result = %v", body["result"])
	}
	if _, ok := body["reason"]; ok {
		t.Fatalf("reason should be omitted on ok, got %v", body["reason"])
	}
	if body["status"] != "success" {
		t.Fatalf("status = %v", body["status"])
	}
}

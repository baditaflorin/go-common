// Package ledger is the canonical client for the fleet's token ledger
// (baditaflorin/go-fleet-token-ledger, ADR-0033). Any service that wants
// to meter a call charges tokens against the CALLER's own balance before
// doing expensive work — never invent a bespoke per-service rate limiter
// or balance table; see CLAUDE.md's cardinal rule: change the library,
// not every service.
//
// Attribution — forward the caller's credential, not your own
//
// A metered charge must land on the account of the human/agent that made
// the original request, not on the service doing the metering. Build a
// Credential from the inbound request with CredentialFromRequest (which
// forwards the exact token middleware.TokenAuthKeystore already verified
// for that request) and pass it to Charge — never use this service's own
// FLEET_API_KEY here. This mirrors how go-fleet-mcp-gateway forwards the
// caller's own Authorization/X-API-Key to upstream services (ADR-0032):
// one revoke still kills access everywhere, ledger included.
//
// Wire format
//
//	POST /v1/charge   Authorization: Bearer <caller's own token>
//	                  body {"amount":N,"reason":"..."}
//	                  200 → charged, deducted from the caller's balance
//	                  402 → insufficient balance; x402-shaped
//	                        PaymentRequirements body (proxy verbatim to
//	                        your own caller with WritePaymentRequired)
//
// Environment
//
//	LEDGER_SERVICE_URL   base URL (default: http://localhost:18208)
//
// Production URL is the fleet-token-ledger registry entry
// (services.json); see private fleet-state/OPS.md for topology.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/middleware"
)

// ErrNoCredential means CredentialFromRequest found no forwardable token
// on the inbound request (e.g. it was authenticated via the private-mesh
// trust path, which requires none). Charge refuses to proceed rather than
// silently charging an empty-string account — that would pool every such
// caller's usage into one shared bucket.
var ErrNoCredential = errors.New("ledger: no caller credential to forward")

// ErrInsufficientBalance means the ledger returned 402. Use
// errors.As(err, &pr) to recover the *PaymentRequired body (already
// x402-shaped) and either inspect Balance/Required or proxy it verbatim
// to your own caller via WritePaymentRequired.
var ErrInsufficientBalance = errors.New("ledger: insufficient balance")

// ErrLedgerUnavailable means the charge call failed transport-wise
// (network error or an unexpected non-200/402 status). The client never
// decides your degrade policy — fail open (serve anyway) or fail closed
// (503) is a per-caller judgment call.
var ErrLedgerUnavailable = errors.New("ledger: unavailable")

// Credential is the caller's own fleet API token, lifted from the inbound
// request that triggered the metered work — never this service's own
// FLEET_API_KEY. Build one with CredentialFromRequest.
type Credential string

// CredentialFromRequest forwards the SAME token
// server.WithKeystoreAuth/WithKeystoreAuthMesh already verified for r,
// via middleware.ExtractToken (the fleet's three canonical shapes:
// Authorization: Bearer, X-API-Key, ?api_key=). Empty when r was
// authenticated via the private-mesh trust path, which carries no token
// at all — Charge rejects an empty Credential with ErrNoCredential rather
// than guessing an account to charge.
func CredentialFromRequest(r *http.Request) Credential {
	return Credential(middleware.ExtractToken(r))
}

// ChargeResult is what a successful (200) charge returns.
type ChargeResult struct {
	Account string `json:"account"`
	Charged int64  `json:"charged"`
	Balance int64  `json:"balance"`
}

// PaymentRequired is the ledger's 402 response. Balance/Required are
// pulled out for callers that want to reason about them programmatically;
// Body is the exact response bytes so WritePaymentRequired can proxy them
// through byte-for-byte without this client re-deriving the x402 shape
// (which may grow fields as real settlement providers land — see
// ADR-0033).
type PaymentRequired struct {
	Balance  int64
	Required int64
	Body     []byte
}

func (p *PaymentRequired) Error() string {
	return fmt.Sprintf("ledger: insufficient balance (have %d, need %d)", p.Balance, p.Required)
}

func (p *PaymentRequired) Unwrap() error { return ErrInsufficientBalance }

// WritePaymentRequired proxies a *PaymentRequired straight through to w —
// the canonical way to pass the ledger's 402 on to your own caller
// without re-deriving its shape.
func WritePaymentRequired(w http.ResponseWriter, pr *PaymentRequired) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write(pr.Body)
}

// Client is a minimal, timeout-bounded HTTP client for the token ledger.
// Construct one per process and reuse — don't churn HTTPClient instances.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}

// New returns a Client wired from env vars.
func New() *Client {
	return &Client{
		BaseURL: env.String("LEDGER_SERVICE_URL", "http://localhost:18208"),
		HTTPClient: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
		UserAgent: "go-common/ledger",
	}
}

type chargeRequestBody struct {
	Amount int64  `json:"amount"`
	Reason string `json:"reason"`
}

// envelope mirrors go-common/response.Response just enough to decode a
// 200 {"status":"success","data":{...}} body without importing the
// response package's Error type (which the ledger's 402 body does not
// use — the 402 shape is x402, not the fleet envelope).
type envelope struct {
	Data ChargeResult `json:"data"`
}

// Charge deducts amount tokens for reason, attributed to the caller
// identified by cred. Returns ErrNoCredential if cred is empty,
// ErrInsufficientBalance (wrapping *PaymentRequired — use errors.As) on
// 402, and ErrLedgerUnavailable for network/5xx failures.
func (c *Client) Charge(ctx context.Context, cred Credential, amount int64, reason string) (*ChargeResult, error) {
	if cred == "" {
		return nil, ErrNoCredential
	}
	body, err := json.Marshal(chargeRequestBody{Amount: amount, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("ledger charge: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/charge", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ledger charge: build req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(cred))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLedgerUnavailable, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrLedgerUnavailable, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var env envelope
		if err := json.Unmarshal(respBody, &env); err != nil {
			return nil, fmt.Errorf("%w: decode response: %v", ErrLedgerUnavailable, err)
		}
		return &env.Data, nil
	case http.StatusPaymentRequired:
		var parsed struct {
			Balance  int64 `json:"balance"`
			Required int64 `json:"required"`
		}
		_ = json.Unmarshal(respBody, &parsed) // best-effort; Body carries the source of truth regardless
		return nil, &PaymentRequired{Balance: parsed.Balance, Required: parsed.Required, Body: respBody}
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrLedgerUnavailable, resp.StatusCode)
	}
}

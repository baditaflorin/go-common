package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCredentialFromRequest_bearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := CredentialFromRequest(r); got != "abc123" {
		t.Fatalf("want abc123, got %q", got)
	}
}

func TestCredentialFromRequest_empty(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := CredentialFromRequest(r); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestCharge_noCredential(t *testing.T) {
	c := &Client{BaseURL: "http://unused", HTTPClient: http.DefaultClient}
	_, err := c.Charge(context.Background(), "", 10, "test")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

func TestCharge_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("want forwarded caller token, got %q", got)
		}
		var body chargeRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Amount != 10 {
			t.Errorf("want amount 10, got %d", body.Amount)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"account": "alice", "charged": 10, "balance": 90},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	res, err := c.Charge(context.Background(), "caller-token", 10, "test")
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if res.Account != "alice" || res.Charged != 10 || res.Balance != 90 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestCharge_insufficientBalance(t *testing.T) {
	const x402Body = `{"x402Version":1,"error":"insufficient_balance","accepts":[{"scheme":"fleet-manual"}],"balance":5,"required":10}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(x402Body))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	_, err := c.Charge(context.Background(), "caller-token", 10, "test")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	var pr *PaymentRequired
	if !errors.As(err, &pr) {
		t.Fatalf("want *PaymentRequired via errors.As, got %T", err)
	}
	if pr.Balance != 5 || pr.Required != 10 {
		t.Fatalf("unexpected PaymentRequired: %+v", pr)
	}
	if string(pr.Body) != x402Body {
		t.Fatalf("Body should be the exact response bytes for verbatim passthrough")
	}
}

func TestWritePaymentRequired_proxiesVerbatim(t *testing.T) {
	pr := &PaymentRequired{Balance: 5, Required: 10, Body: []byte(`{"x402Version":1}`)}
	w := httptest.NewRecorder()
	WritePaymentRequired(w, pr)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d", w.Code)
	}
	if w.Body.String() != `{"x402Version":1}` {
		t.Fatalf("want verbatim body, got %s", w.Body.String())
	}
}

func TestCharge_unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	_, err := c.Charge(context.Background(), "caller-token", 10, "test")
	if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("want ErrLedgerUnavailable, got %v", err)
	}
}

func TestBalance_noCredential(t *testing.T) {
	c := &Client{BaseURL: "http://unused", HTTPClient: http.DefaultClient}
	_, err := c.Balance(context.Background(), "")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

func TestBalance_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("want GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/balance" {
			t.Errorf("want /v1/balance, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("want forwarded caller token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"account": "alice", "balance": 42},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	balance, err := c.Balance(context.Background(), "caller-token")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != 42 {
		t.Fatalf("want balance 42, got %d", balance)
	}
}

func TestBalance_doesNotMutateState(t *testing.T) {
	// Balance must never be POST /v1/charge in disguise — assert the
	// client only ever issues a GET, no request body.
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"account": "alice", "balance": 0},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	if _, err := c.Balance(context.Background(), "caller-token"); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("want GET, got %s", gotMethod)
	}
	if gotBody != "" {
		t.Fatalf("want empty body, got %q", gotBody)
	}
}

func TestBalance_unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	_, err := c.Balance(context.Background(), "caller-token")
	if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("want ErrLedgerUnavailable, got %v", err)
	}
}

func TestBalance_timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := c.Balance(ctx, "caller-token")
	if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("want ErrLedgerUnavailable on timeout, got %v", err)
	}
}

func TestBalance_malformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: http.DefaultClient, UserAgent: "test"}
	_, err := c.Balance(context.Background(), "caller-token")
	if !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("want ErrLedgerUnavailable on malformed body, got %v", err)
	}
}

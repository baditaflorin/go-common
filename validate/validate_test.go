package validate_test

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	fleetErrors "github.com/baditaflorin/go-common/errors"
	"github.com/baditaflorin/go-common/validate"
)

type CreateReq struct {
	Name  string `json:"name"  validate:"required,max=64"`
	Email string `json:"email" validate:"required,email"`
	Age   int    `json:"age"   validate:"min=0,max=150"`
	Role  string `json:"role"  validate:"oneof=admin|user|viewer"`
}

// bindJSON returns *fleetErrors.Error directly (not error interface) so that
// nil comparison works correctly — a nil *fleetErrors.Error assigned to an
// error interface would be non-nil (classic Go nil-interface gotcha).
func bindJSON(body string, dst any) *fleetErrors.Error {
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return validate.Bind(r, dst)
}

func TestBindValid(t *testing.T) {
	var req CreateReq
	err := bindJSON(`{"name":"Alice","email":"alice@example.com","age":30,"role":"admin"}`, &req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "Alice" {
		t.Fatalf("name: got %q", req.Name)
	}
}

func TestBindMissingRequired(t *testing.T) {
	var req CreateReq
	err := bindJSON(`{"email":"a@b.com","age":20,"role":"user"}`, &req)
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected 'required' in error, got: %v", err)
	}
}

func TestBindMaxLength(t *testing.T) {
	var req struct {
		Name string `json:"name" validate:"max=5"`
	}
	err := bindJSON(`{"name":"toolongname"}`, &req)
	if err == nil {
		t.Fatal("expected error for max length violation")
	}
}

func TestBindEmailInvalid(t *testing.T) {
	var req struct {
		Email string `json:"email" validate:"email"`
	}
	err := bindJSON(`{"email":"not-an-email"}`, &req)
	if err == nil {
		t.Fatal("expected error for invalid email")
	}
}

func TestBindEmailValid(t *testing.T) {
	var req struct {
		Email string `json:"email" validate:"email"`
	}
	err := bindJSON(`{"email":"user@example.com"}`, &req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBindOneOf(t *testing.T) {
	var req struct {
		Role string `json:"role" validate:"oneof=admin|user"`
	}
	err := bindJSON(`{"role":"superuser"}`, &req)
	if err == nil {
		t.Fatal("expected error for invalid oneof value")
	}
	err2 := bindJSON(`{"role":"admin"}`, &req)
	if err2 != nil {
		t.Fatalf("unexpected error for valid oneof: %v", err2)
	}
}

func TestBindPattern(t *testing.T) {
	var req struct {
		Code string `json:"code" validate:"pattern=^[A-Z]{3}$"`
	}
	err := bindJSON(`{"code":"abc"}`, &req) // lowercase fails
	if err == nil {
		t.Fatal("expected error for pattern mismatch")
	}
	err2 := bindJSON(`{"code":"ABC"}`, &req)
	if err2 != nil {
		t.Fatalf("unexpected error for pattern match: %v", err2)
	}
}

func TestBindURL(t *testing.T) {
	var req struct {
		URL string `json:"url" validate:"url"`
	}
	if err := bindJSON(`{"url":"ftp://bad.com"}`, &req); err == nil {
		t.Fatal("expected error for ftp:// URL")
	}
	if err := bindJSON(`{"url":"https://good.com/path"}`, &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBindMinInt(t *testing.T) {
	var req struct {
		Count int `json:"count" validate:"min=1"`
	}
	if err := bindJSON(`{"count":0}`, &req); err == nil {
		t.Fatal("expected error for count < min")
	}
	if err := bindJSON(`{"count":5}`, &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBindBytesValid(t *testing.T) {
	var req struct {
		Name string `json:"name" validate:"required"`
	}
	if err := validate.BindBytes([]byte(`{"name":"test"}`), &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStructMultipleErrors(t *testing.T) {
	type R struct {
		A string `json:"a" validate:"required"`
		B string `json:"b" validate:"required"`
	}
	err := validate.Struct(&R{})
	if err == nil {
		t.Fatal("expected error for two missing fields")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("expected both fields in error, got: %v", err)
	}
}

func TestBindInvalidJSON(t *testing.T) {
	r := httptest.NewRequest("POST", "/", bytes.NewBufferString(`{bad json}`))
	var req struct{ Name string }
	if err := validate.Bind(r, &req); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

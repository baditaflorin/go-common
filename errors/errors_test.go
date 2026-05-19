package errors_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/errors"
)

func TestNewError(t *testing.T) {
	e := errors.New(http.StatusNotFound, "not_found", "item missing")
	if e.Status != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", e.Status, http.StatusNotFound)
	}
	if e.Code != "not_found" {
		t.Fatalf("code: got %q, want %q", e.Code, "not_found")
	}
	if e.Msg != "item missing" {
		t.Fatalf("msg: got %q", e.Msg)
	}
}

func TestErrorString(t *testing.T) {
	e := errors.New(400, "bad_request", "invalid input")
	got := e.Error()
	if got != "bad_request: invalid input" {
		t.Fatalf("got %q", got)
	}
}

func TestErrorStringWithCause(t *testing.T) {
	cause := fmt.Errorf("underlying io error")
	e := errors.Wrap(cause, 500, "internal", "disk failure")
	got := e.Error()
	want := "internal: disk failure: underlying io error"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTTPStatus(t *testing.T) {
	e := &errors.Error{}
	if e.HTTPStatus() != http.StatusInternalServerError {
		t.Fatal("empty Error should default to 500")
	}
	e.Status = http.StatusForbidden
	if e.HTTPStatus() != http.StatusForbidden {
		t.Fatal("should return set status")
	}
}

func TestUnwrap(t *testing.T) {
	cause := fmt.Errorf("root")
	e := errors.Wrap(cause, 500, "internal", "outer")
	if errors.Unwrap(e) != cause {
		t.Fatal("Unwrap should return cause")
	}
}

func TestIs(t *testing.T) {
	sentinel := errors.New(404, "not_found", "")
	actual := errors.New(404, "not_found", "specific message")
	if !errors.Is(actual, sentinel) {
		t.Fatal("Is should match on Code")
	}
	other := errors.New(400, "bad_request", "")
	if errors.Is(actual, other) {
		t.Fatal("Is should not match different codes")
	}
}

func TestFromError(t *testing.T) {
	original := errors.New(403, "auth.forbidden", "nope")
	chain := fmt.Errorf("outer: %w", original)
	got, ok := errors.FromError(chain)
	if !ok {
		t.Fatal("expected ok=true for wrapped *Error")
	}
	if got.Code != "auth.forbidden" {
		t.Fatalf("code: got %q", got.Code)
	}
}

func TestFromErrorStdlib(t *testing.T) {
	err := fmt.Errorf("plain error")
	got, ok := errors.FromError(err)
	if ok {
		t.Fatal("expected ok=false for plain error")
	}
	if got.Status != http.StatusInternalServerError {
		t.Fatal("plain error should map to 500")
	}
}

func TestNewf(t *testing.T) {
	e := errors.Newf(400, "bad_request", "field %q is required", "name")
	if e.Msg != `field "name" is required` {
		t.Fatalf("got %q", e.Msg)
	}
}

func TestSentinels(t *testing.T) {
	sentinels := []*errors.Error{
		errors.ErrNotFound,
		errors.ErrBadRequest,
		errors.ErrUnauthorized,
		errors.ErrForbidden,
		errors.ErrConflict,
		errors.ErrInternal,
		errors.ErrUnavailable,
		errors.ErrTimeout,
	}
	for _, s := range sentinels {
		if s.Code == "" {
			t.Fatalf("sentinel has empty code: %+v", s)
		}
		if s.Status == 0 {
			t.Fatalf("sentinel has zero status: %+v", s)
		}
	}
}

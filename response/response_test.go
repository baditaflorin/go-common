package response

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSuccess(t *testing.T) {
	r := Success(map[string]int{"n": 1})
	if r.Status != "success" || r.Error != nil || r.Data == nil {
		t.Fatalf("Success: %+v", r)
	}
}

func TestErrorResp(t *testing.T) {
	r := ErrorResp(http.StatusNotFound, "no such thing")
	if r.Status != "error" || r.Data != nil {
		t.Fatalf("ErrorResp envelope: %+v", r)
	}
	if r.Error == nil || r.Error.Code != 404 || r.Error.Message != "no such thing" || r.Error.ErrorCode != "" {
		t.Fatalf("ErrorResp error: %+v", r.Error)
	}
}

func TestNewError(t *testing.T) {
	r := NewError(http.StatusForbidden, "auth.scope_mismatch", "token scope does not cover this resource")
	if r.Status != "error" || r.Error == nil {
		t.Fatalf("NewError: %+v", r)
	}
	if r.Error.Code != 403 || r.Error.ErrorCode != "auth.scope_mismatch" {
		t.Fatalf("NewError code fields: %+v", r.Error)
	}
}

func TestFromError(t *testing.T) {
	if r := FromError(nil); r.Status != "success" || r.Data != nil || r.Error != nil {
		t.Fatalf("FromError(nil) = %+v, want an empty success", r)
	}
	r := FromError(errors.New("disk on fire"))
	if r.Status != "error" || r.Error == nil {
		t.Fatalf("FromError(err) = %+v", r)
	}
	if r.Error.Code != 500 || r.Error.ErrorCode != "internal" || r.Error.Message != "disk on fire" {
		t.Fatalf("FromError(err) error = %+v", r.Error)
	}
}

func TestErrorString(t *testing.T) {
	var nilErr *Error
	cases := []struct {
		name string
		e    *Error
		want string
	}{
		{"nil", nilErr, "<nil>"},
		{"with code", &Error{Code: 403, ErrorCode: "auth.scope_mismatch", Message: "no"}, "auth.scope_mismatch (http 403): no"},
		{"without code", &Error{Code: 404, Message: "gone"}, "http 404: gone"},
	}
	for _, c := range cases {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%s: Error() = %q, want %q", c.name, got, c.want)
		}
	}
}

type recordingObserver struct{ events []Event }

func (o *recordingObserver) ObserveEnvelope(ev Event) { o.events = append(o.events, ev) }

// The process-wide observer is installed, seen by Envelope, and removable. It is
// global, so the test restores whatever was there and does not run in parallel.
func TestDefaultObserver(t *testing.T) {
	prev := DefaultObserver()
	t.Cleanup(func() {
		if prev == nil {
			SetDefaultObserver(nil)
		} else {
			SetDefaultObserver(prev)
		}
	})
	clearIdentity(t)
	fixedClock(t)

	SetDefaultObserver(nil)
	if DefaultObserver() != nil {
		t.Fatal("DefaultObserver after SetDefaultObserver(nil) is not nil")
	}
	Envelope(nil, 1) // no observer: must not panic

	rec := &recordingObserver{}
	SetDefaultObserver(rec)
	if DefaultObserver() == nil {
		t.Fatal("DefaultObserver is nil after SetDefaultObserver(rec)")
	}
	SetServiceID("svc-a")
	t.Cleanup(func() { SetServiceID("") })
	Envelope(map[string]any{"x": 1}, 3)
	Envelope(map[string]any{"_service": "other"}, 0) // a reserved key: the event carries a warning
	if len(rec.events) != 2 {
		t.Fatalf("observer saw %d events, want 2", len(rec.events))
	}
	if ev := rec.events[0]; ev.Service != "svc-a" || ev.SchemaVersion != 3 || ev.Warning != "" {
		t.Fatalf("first event = %+v", ev)
	}
	if ev := rec.events[1]; ev.Warning == "" || !strings.Contains(ev.Warning, "_service") {
		t.Fatalf("second event should warn about the reserved key: %+v", ev)
	}

	SetDefaultObserver(nil)
	Envelope(nil, 0)
	if len(rec.events) != 2 {
		t.Fatalf("a removed observer still received events: %d", len(rec.events))
	}
}

package selftest

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsLive_RoundTrip(t *testing.T) {
	// Default mode (?live=0 / absent): IsLive must be false in every
	// check ctx so existing services see no behavior change.
	suite := NewSuite("svc", "v1")
	saw := struct {
		def  bool
		live bool
	}{}
	suite.Check("probe", func(ctx context.Context) error {
		if IsLive(ctx) {
			saw.live = true
		} else {
			saw.def = true
		}
		return nil
	})

	r := httptest.NewRequest("GET", "/selftest", nil)
	w := httptest.NewRecorder()
	suite.Render(w, r)
	if !saw.def || saw.live {
		t.Fatalf("default mode: want IsLive=false; saw def=%v live=%v", saw.def, saw.live)
	}

	r2 := httptest.NewRequest("GET", "/selftest?live=1", nil)
	w2 := httptest.NewRecorder()
	saw.def, saw.live = false, false
	suite.Render(w2, r2)
	if saw.def || !saw.live {
		t.Fatalf("live=1: want IsLive=true; saw def=%v live=%v", saw.def, saw.live)
	}
}

func TestIsLive_NilContext(t *testing.T) {
	// Defensive: IsLive on a nil ctx must not panic.
	if IsLive(nil) {
		t.Fatal("IsLive(nil) returned true; want false")
	}
}

func TestIsLive_OnlyAcceptsLiteralOne(t *testing.T) {
	// ?live=true, ?live=yes etc. are NOT live mode — we only accept
	// the literal "1" so a stray query string doesn't flip behavior.
	for _, q := range []string{"", "0", "true", "yes", "on"} {
		suite := NewSuite("svc", "v1")
		var live bool
		suite.Check("probe", func(ctx context.Context) error {
			live = IsLive(ctx)
			return nil
		})
		url := "/selftest"
		if q != "" {
			url += "?live=" + q
		}
		r := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		suite.Render(w, r)
		if live {
			t.Fatalf("live=%q: want IsLive=false; got true", q)
		}
		if !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("live=%q: response not OK: %s", q, w.Body.String())
		}
	}
}

package client

import (
	"net/http/httptest"
	"testing"
)

func TestOptionsFromQuery(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantMode  string
		wantCount int
	}{
		{"no flags", "/x", "static", 0},
		{"use_js=true", "/x?use_js=true", "rendered_dom", 1},
		{"use_js=1", "/x?use_js=1", "rendered_dom", 1},
		{"use_js=yes", "/x?use_js=yes", "rendered_dom", 1},
		{"use_js=on (capital)", "/x?use_js=ON", "rendered_dom", 1},
		{"use_js=false", "/x?use_js=false", "static", 0},
		{"use_network=true", "/x?use_network=true", "rendered_network", 1},
		{"use_network=true overrides use_js=false", "/x?use_js=false&use_network=true", "rendered_network", 1},
		{"both set, network wins", "/x?use_js=true&use_network=true", "rendered_network", 1},
		{"unrecognised flag ignored", "/x?frobnicate=42", "static", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			if got := ModeFromQuery(r); got != tc.wantMode {
				t.Errorf("ModeFromQuery: got %q want %q", got, tc.wantMode)
			}
			if got := len(OptionsFromQuery(r)); got != tc.wantCount {
				t.Errorf("OptionsFromQuery count: got %d want %d", got, tc.wantCount)
			}
		})
	}
}

func TestOptionsFromQuery_NilSafe(t *testing.T) {
	if got := OptionsFromQuery(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil request should give empty (not nil) slice, got %v", got)
	}
	if got := ModeFromQuery(nil); got != "static" {
		t.Fatalf("nil request mode should be static, got %q", got)
	}
}

func TestFetchCapabilities_Declared(t *testing.T) {
	if len(FetchCapabilities) < 2 {
		t.Fatalf("expected at least use_js + use_network in FetchCapabilities, got %d", len(FetchCapabilities))
	}
	names := map[string]bool{}
	for _, c := range FetchCapabilities {
		names[c.Name] = true
	}
	for _, want := range []string{"use_js", "use_network"} {
		if !names[want] {
			t.Errorf("FetchCapabilities missing %q", want)
		}
	}
}

package client

import (
	"testing"
)

func fixture() *FetchResult {
	return &FetchResult{
		URL:      "https://example.com",
		FinalURL: "https://example.com/app",
		Network: []NetworkEntry{
			{
				URL: "https://example.com/app", Method: "GET", Status: 200,
				ResourceType:    "document",
				ResponseHeaders: map[string]string{"Content-Type": "text/html"},
			},
			{
				URL: "https://example.com/static/main.abc.js", Method: "GET", Status: 200,
				ResourceType:    "script",
				ResponseSize:    180_000,
				ResponseHeaders: map[string]string{"Content-Type": "application/javascript", "SourceMap": "main.abc.js.map"},
			},
			{
				URL: "https://example.com/static/runtime.js", Method: "GET", Status: 200,
				ResourceType:    "script",
				ResponseSize:    4_000,
				ResponseHeaders: map[string]string{"Content-Type": "application/javascript"},
			},
			{
				URL: "https://cdn.third.party/widget.js", Method: "GET", Status: 200,
				ResourceType: "script",
				ResponseSize: 9_000,
				ResponseHeaders: map[string]string{
					"Content-Type": "application/javascript",
					"Set-Cookie":   "tracker=xyz; Domain=third.party; HttpOnly",
				},
			},
			{
				URL: "https://example.com/api/users/42/profile", Method: "GET", Status: 200,
				ResourceType:    "xhr",
				ResponseHeaders: map[string]string{"Content-Type": "application/json"},
			},
			{
				URL: "https://example.com/api/users/43/profile", Method: "GET", Status: 200,
				ResourceType:    "xhr",
				ResponseHeaders: map[string]string{"Content-Type": "application/json"},
			},
			{
				URL: "https://example.com/graphql", Method: "POST", Status: 200,
				ResourceType:    "fetch",
				ResponseHeaders: map[string]string{"Content-Type": "application/json"},
			},
			{
				URL: "https://example.com/login?next=/dashboard&unrelated=x", Method: "GET", Status: 302,
				ResourceType: "document",
				ResponseHeaders: map[string]string{
					"Set-Cookie": "session=abc; HttpOnly; Secure",
					"Location":   "/dashboard",
				},
			},
			{
				URL: "https://embed.example.com/widget", Method: "GET", Status: 200,
				ResourceType: "document",
			},
		},
		ConsoleLogs: []string{
			"React is running in development mode",
			"INFO: route changed",
			"Uncaught TypeError: x is undefined",
		},
	}
}

func TestXHREndpoints_TemplatisesAndDedupes(t *testing.T) {
	got := XHREndpoints(fixture())
	if len(got) != 2 {
		t.Fatalf("expected 2 deduped endpoints, got %d: %+v", len(got), got)
	}
	wantPaths := map[string]bool{
		"https://example.com/api/users/{id}/profile": false,
		"https://example.com/graphql":                false,
	}
	for _, e := range got {
		if _, ok := wantPaths[e.URL]; !ok {
			t.Errorf("unexpected endpoint URL: %q", e.URL)
			continue
		}
		wantPaths[e.URL] = true
	}
	for k, v := range wantPaths {
		if !v {
			t.Errorf("missing endpoint: %s", k)
		}
	}
}

func TestGraphQLEndpoints_FindsPOSTJSON(t *testing.T) {
	got := GraphQLEndpoints(fixture())
	if len(got) != 1 || got[0].Method != "POST" {
		t.Fatalf("expected one POST graphql endpoint, got %+v", got)
	}
}

func TestJSAssets_OnlyScripts(t *testing.T) {
	got := JSAssets(fixture())
	if len(got) != 3 {
		t.Fatalf("expected 3 script entries, got %d", len(got))
	}
}

func TestSourcemapCandidates_HeaderSniff(t *testing.T) {
	got := SourcemapCandidates(fixture())
	if len(got) != 1 {
		t.Fatalf("expected 1 sourcemap candidate, got %d", len(got))
	}
}

func TestIframeURLs(t *testing.T) {
	got := IframeURLs(fixture())
	if len(got) != 2 { // /login (302 doc) and embed.example.com/widget
		t.Fatalf("expected 2 iframe-ish docs, got %d: %v", len(got), got)
	}
}

func TestRedirectParams(t *testing.T) {
	got := RedirectParams(fixture())
	if len(got) != 1 || got[0].Key != "next" || got[0].Value != "/dashboard" {
		t.Fatalf("expected one next=/dashboard, got %+v", got)
	}
}

func TestSetCookiesAll_AcrossHops(t *testing.T) {
	got := SetCookiesAll(fixture())
	if len(got) < 2 {
		t.Fatalf("expected at least 2 cookies (login session + tracker), got %d", len(got))
	}
}

func TestConsoleErrors(t *testing.T) {
	got := ConsoleErrors(fixture())
	if len(got) != 2 {
		t.Fatalf("expected 2 error-ish lines (dev mode warning + uncaught), got %d: %v", len(got), got)
	}
}

func TestFilters_Composable(t *testing.T) {
	f := fixture()
	thirdParty := ByHostSuffix(f.Network, "third.party")
	if len(thirdParty) != 1 || thirdParty[0].URL != "https://cdn.third.party/widget.js" {
		t.Fatalf("ByHostSuffix wrong: %+v", thirdParty)
	}
	scripts := ByResourceType(f.Network, "script")
	big := BySizeGreaterThan(scripts, 10_000)
	if len(big) != 1 || big[0].URL != "https://example.com/static/main.abc.js" {
		t.Fatalf("composed filter wrong: %+v", big)
	}
	redirects := ByStatusClass(f.Network, 3)
	if len(redirects) != 1 || redirects[0].Status != 302 {
		t.Fatalf("ByStatusClass wrong: %+v", redirects)
	}
	posts := ByMethod(f.Network, "POST")
	if len(posts) != 1 {
		t.Fatalf("ByMethod wrong: %+v", posts)
	}
	json := ByContentType(f.Network, "application/json")
	if len(json) != 3 { // both /api/users hops + /graphql
		t.Fatalf("ByContentType wrong: %d %+v", len(json), json)
	}
}

func TestHelpers_NilSafe(t *testing.T) {
	var r *FetchResult
	if XHREndpoints(r) != nil ||
		JSAssets(r) != nil ||
		SourcemapCandidates(r) != nil ||
		IframeURLs(r) != nil ||
		RedirectParams(r) != nil ||
		SetCookiesAll(r) != nil ||
		ConsoleErrors(r) != nil {
		t.Fatal("helpers should return nil/empty for nil input")
	}
	if r.HasNetwork() {
		t.Fatal("HasNetwork should be false on nil")
	}
}

func TestLooksLikeUUID(t *testing.T) {
	if !looksLikeUUID("550e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("valid uuid not detected")
	}
	if looksLikeUUID("not-a-uuid") {
		t.Fatal("non-uuid wrongly detected")
	}
}

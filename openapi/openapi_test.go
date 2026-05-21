package openapi

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
)

// TestNew verifies that New() returns a spec pre-populated with the four
// standard fleet endpoints.
func TestNew(t *testing.T) {
	spec := New("test-service", "1.0.0")

	if spec.OpenAPI != "3.0.3" {
		t.Errorf("OpenAPI version: got %q, want %q", spec.OpenAPI, "3.0.3")
	}
	if spec.Info.Title != "test-service" {
		t.Errorf("Info.Title: got %q, want %q", spec.Info.Title, "test-service")
	}
	if spec.Info.Version != "1.0.0" {
		t.Errorf("Info.Version: got %q, want %q", spec.Info.Version, "1.0.0")
	}

	required := []string{"/health", "/version", "/selftest", "/openapi.json"}
	for _, path := range required {
		item, ok := spec.Paths[path]
		if !ok {
			t.Errorf("missing required path %q", path)
			continue
		}
		if item.Get == nil {
			t.Errorf("path %q: expected GET operation, got nil", path)
		}
	}
}

// TestAddRoute verifies AddRoute adds a path+method to the spec.
func TestAddRoute(t *testing.T) {
	spec := New("svc", "0.1")

	spec.AddRoute("GET", "/example", Operation{
		Summary: "example endpoint",
		Responses: map[string]Response{
			"200": {Description: "ok"},
		},
	})

	item, ok := spec.Paths["/example"]
	if !ok {
		t.Fatal("expected /example in paths after AddRoute")
	}
	if item.Get == nil {
		t.Fatal("expected GET operation on /example")
	}
	if item.Get.Summary != "example endpoint" {
		t.Errorf("summary: got %q, want %q", item.Get.Summary, "example endpoint")
	}
}

// TestAddRouteCaseInsensitive confirms method matching is case-insensitive.
func TestAddRouteCaseInsensitive(t *testing.T) {
	spec := New("svc", "0.1")

	spec.AddRoute("post", "/submit", Operation{
		Responses: map[string]Response{"200": {Description: "ok"}},
	})
	if spec.Paths["/submit"].Post == nil {
		t.Error("expected POST operation when method passed as lowercase 'post'")
	}
}

// TestJSON verifies JSON() returns valid, parseable JSON containing the spec.
func TestJSON(t *testing.T) {
	spec := New("svc", "0.1")

	data, err := spec.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JSON() output is not valid JSON: %v", err)
	}

	if out["openapi"] != "3.0.3" {
		t.Errorf("JSON openapi field: got %v, want 3.0.3", out["openapi"])
	}

	paths, ok := out["paths"].(map[string]interface{})
	if !ok {
		t.Fatal("JSON paths field is missing or wrong type")
	}
	if _, found := paths["/health"]; !found {
		t.Error("JSON paths missing /health")
	}
}

// TestScanner exercises the comment-annotation scanner with synthetic Go
// source bytes (no filesystem access required).
func TestScanner(t *testing.T) {
	src := `package handlers

// @openapi GET /ping
// @summary Ping check
// @tag internal
// @response 200 {"pong":true}
func handlePing(w http.ResponseWriter, r *http.Request) {}

// @openapi POST /submit
// @summary Submit data
// @tag data
func handleSubmit(w http.ResponseWriter, r *http.Request) {}
`

	spec := New("svc", "0.1")
	n := scanReader(bufio.NewScanner(strings.NewReader(src)), spec)

	if n != 2 {
		t.Errorf("scanReader returned %d routes added, want 2", n)
	}

	pingItem, ok := spec.Paths["/ping"]
	if !ok {
		t.Fatal("expected /ping in spec after scanning")
	}
	if pingItem.Get == nil {
		t.Fatal("expected GET on /ping")
	}
	if pingItem.Get.Summary != "Ping check" {
		t.Errorf("summary: got %q, want %q", pingItem.Get.Summary, "Ping check")
	}
	if len(pingItem.Get.Tags) != 1 || pingItem.Get.Tags[0] != "internal" {
		t.Errorf("tags: got %v, want [internal]", pingItem.Get.Tags)
	}
	if resp, ok := pingItem.Get.Responses["200"]; !ok || resp.Description == "" {
		t.Error("expected non-empty 200 response description on /ping")
	}

	submitItem, ok := spec.Paths["/submit"]
	if !ok {
		t.Fatal("expected /submit in spec after scanning")
	}
	if submitItem.Post == nil {
		t.Fatal("expected POST on /submit")
	}
}

// TestScannerParam verifies @param lines are parsed and attached to the
// operation as OpenAPI Parameter objects.
func TestScannerParam(t *testing.T) {
	src := `package handlers

// @openapi GET /check
// @summary Check something
// @tag tool
// @param query url string true URL to check
// @param query target string false Alias for url
// @response 200 {"ok":true}
func handleCheck(w http.ResponseWriter, r *http.Request) {}
`
	spec := New("svc", "0.1")
	n := scanReader(bufio.NewScanner(strings.NewReader(src)), spec)

	if n != 1 {
		t.Fatalf("expected 1 route, got %d", n)
	}
	item, ok := spec.Paths["/check"]
	if !ok {
		t.Fatal("expected /check in spec")
	}
	op := item.Get
	if op == nil {
		t.Fatal("expected GET on /check")
	}
	if len(op.Parameters) != 2 {
		t.Fatalf("expected 2 params, got %d: %+v", len(op.Parameters), op.Parameters)
	}
	url := op.Parameters[0]
	if url.Name != "url" || url.In != "query" || !url.Required || url.Schema == nil || url.Schema.Type != "string" {
		t.Errorf("param[0] wrong: %+v", url)
	}
	target := op.Parameters[1]
	if target.Name != "target" || target.In != "query" || target.Required {
		t.Errorf("param[1] wrong: %+v", target)
	}
	if target.Description != "Alias for url" {
		t.Errorf("param[1] description: got %q, want %q", target.Description, "Alias for url")
	}
}

// TestScannerMalformedAnnotation ensures malformed @openapi lines are
// silently skipped without panicking.
func TestScannerMalformedAnnotation(t *testing.T) {
	src := `// @openapi ONLYONEWORD
// @summary whatever
func f() {}
`
	spec := New("svc", "0.1")
	n := scanReader(bufio.NewScanner(strings.NewReader(src)), spec)
	if n != 0 {
		t.Errorf("expected 0 routes from malformed annotation, got %d", n)
	}
}

// TestScannerFileEndInBlock ensures an annotation at EOF is flushed.
func TestScannerFileEndInBlock(t *testing.T) {
	// No trailing non-comment line after the annotation.
	src := `// @openapi DELETE /item
// @summary Remove an item`

	spec := New("svc", "0.1")
	n := scanReader(bufio.NewScanner(strings.NewReader(src)), spec)
	if n != 1 {
		t.Errorf("expected 1 route flushed at EOF, got %d", n)
	}
	if spec.Paths["/item"].Delete == nil {
		t.Error("expected DELETE on /item after EOF flush")
	}
}

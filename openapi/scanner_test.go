package openapi

import (
	"os"
	"path/filepath"
	"testing"
)

const annotated = `package handlers

import "net/http"

// @openapi GET /example
// @summary Returns an example
// @tag example
// @param query url string true URL to scan
// @response 200 {"message":"string"}
func handleExample(w http.ResponseWriter, r *http.Request) {}

// @openapi POST /things
func handleThing(w http.ResponseWriter, r *http.Request) {}
`

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanDir(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), annotated)
	write(t, filepath.Join(dir, "sub", "b.go"), "package sub\n\n// @openapi DELETE /sub/{id}\nfunc del() {}\n")
	write(t, filepath.Join(dir, "notes.txt"), "// @openapi GET /not-go\n")
	write(t, filepath.Join(dir, ".hidden", "c.go"), "package h\n\n// @openapi GET /hidden\nfunc h() {}\n")
	write(t, filepath.Join(dir, "vendor", "d.go"), "package v\n\n// @openapi GET /vendored\nfunc v() {}\n")

	spec := New("svc", "1.0.0")
	n, err := ScanDir(dir, spec)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if n != 3 {
		t.Fatalf("ScanDir added %d routes, want 3 (a.go has two, sub/b.go one; hidden, vendor and non-Go files are skipped)", n)
	}

	get := spec.Paths["/example"].Get
	if get == nil || get.Summary != "Returns an example" || len(get.Tags) != 1 || get.Tags[0] != "example" {
		t.Fatalf("GET /example = %+v", get)
	}
	if len(get.Parameters) != 1 || get.Parameters[0].Name != "url" || !get.Parameters[0].Required {
		t.Fatalf("GET /example parameters = %+v", get.Parameters)
	}
	if spec.Paths["/things"].Post == nil {
		t.Fatal("POST /things was not added")
	}
	if got := spec.Paths["/things"].Post.Responses["200"].Description; got != "OK" {
		t.Fatalf("an annotation with no @response gets a default 200, got %q", got)
	}
	if spec.Paths["/sub/{id}"].Delete == nil {
		t.Fatal("DELETE /sub/{id} was not added")
	}
	for _, p := range []string{"/not-go", "/hidden", "/vendored"} {
		if _, ok := spec.Paths[p]; ok {
			t.Errorf("%s should not have been scanned", p)
		}
	}
}

func TestScanDirErrors(t *testing.T) {
	spec := New("svc", "1.0.0")
	if _, err := ScanDir(filepath.Join(t.TempDir(), "missing"), spec); err == nil {
		t.Fatal("ScanDir of a missing directory returned no error")
	}

	// An unreadable Go file is reported, not skipped.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.go")
	write(t, bad, annotated)
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	if f, err := os.Open(bad); err == nil {
		f.Close()
		t.Skip("running as a user that can read a mode 000 file")
	}
	if _, err := ScanDir(dir, spec); err == nil {
		t.Fatal("ScanDir of an unreadable file returned no error")
	}
}

func TestAddRouteMethods(t *testing.T) {
	spec := New("svc", "1.0.0")
	op := Operation{Summary: "x"}
	for _, m := range []string{"get", "POST", "Put", "delete", "PATCH"} {
		spec.AddRoute(m, "/r", op)
	}
	item := spec.Paths["/r"]
	if item.Get == nil || item.Post == nil || item.Put == nil || item.Delete == nil || item.Patch == nil {
		t.Fatalf("AddRoute did not set every method: %+v", item)
	}
	// An unknown method adds the path but no operation, and does not disturb the others.
	spec.AddRoute("TRACE", "/r", Operation{Summary: "y"})
	if spec.Paths["/r"].Get.Summary != "x" {
		t.Fatal("an unknown method changed an existing operation")
	}
}

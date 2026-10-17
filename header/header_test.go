package header

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// headerConsts returns every string constant declared in header.go.
func headerConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "header.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				out[name.Name] = v
			}
		}
	}
	return out
}

// headerName is the shape every constant must have: letters and digits joined by
// single hyphens. A space, colon or underscore is a typo, and nginx drops a
// header whose name contains an underscore unless told otherwise, which turns a
// typo into a silently missing auth header.
var headerName = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// One mistyped header name is a silent auth bypass, so every constant has to be
// a well-formed header name, and no two may name the same header. net/http
// canonicalises names on Set and Get, so spellings such as X-API-Key are fine; a
// second constant for "x-api-key" is not.
func TestConstantsAreWellFormedAndUnique(t *testing.T) {
	t.Parallel()
	consts := headerConsts(t)
	if len(consts) < 5 {
		t.Fatalf("found only %d header constants; is header.go still parsed correctly?", len(consts))
	}
	seen := map[string]string{}
	for name, v := range consts {
		if strings.Contains(v, "/") {
			continue // the ContentType* media types are header values, not names
		}
		if !headerName.MatchString(v) {
			t.Errorf("%s = %q is not a well-formed header name", name, v)
		}
		key := strings.ToLower(v)
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s both name the header %q", name, other, v)
		}
		seen[key] = name
	}
}

func TestInboundAuthHeadersKeepTheirWireNames(t *testing.T) {
	t.Parallel()
	// These are an interface with the gateway; renaming one breaks auth for every service.
	for got, want := range map[string]string{
		AuthUser:  "X-Auth-User",
		AuthScope: "X-Auth-Scope",
		AuthTier:  "X-Auth-Tier",
	} {
		if got != want {
			t.Errorf("header constant = %q, want %q", got, want)
		}
	}
}

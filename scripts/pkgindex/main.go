// Command pkgindex generates docs/PACKAGES.md, the index of every importable
// go-common package, from the packages' own doc comments.
//
//	go run ./scripts/pkgindex           write docs/PACKAGES.md
//	go run ./scripts/pkgindex -check    exit 1 if docs/PACKAGES.md is stale
//
// A package without a doc comment is an error in both modes: the index is only
// as useful as the comments it is built from, and a new package should have to
// say what it is for. It reads source only (go/parser, no type checking), so it
// is fast and needs no dependencies.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const outFile = "docs/PACKAGES.md"

type pkg struct {
	dir        string // slash-separated, relative to the module root
	summary    string
	deprecated bool
}

func main() {
	root := flag.String("root", ".", "module root")
	check := flag.Bool("check", false, "fail if "+outFile+" is out of date instead of writing it")
	flag.Parse()

	module, err := modulePath(filepath.Join(*root, "go.mod"))
	if err != nil {
		fatal(err)
	}
	pkgs, missing, err := scan(*root)
	if err != nil {
		fatal(err)
	}

	want := render(module, pkgs)
	path := filepath.Join(*root, filepath.FromSlash(outFile))
	stale := false
	if *check {
		got, _ := os.ReadFile(path)
		stale = !bytes.Equal(got, want)
	} else if err := os.WriteFile(path, want, 0o644); err != nil {
		fatal(err)
	}

	for _, m := range missing {
		fmt.Fprintf(os.Stderr, "pkgindex: package %s has no package comment; add one (a doc.go is fine)\n", m)
	}
	if stale {
		fmt.Fprintf(os.Stderr, "pkgindex: %s is stale; run `go run ./scripts/pkgindex` and commit the result\n", outFile)
	}
	if stale || len(missing) > 0 {
		os.Exit(1)
	}
	if !*check {
		fmt.Printf("pkgindex: wrote %s (%d packages)\n", outFile, len(pkgs))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "pkgindex:", err)
	os.Exit(1)
}

func modulePath(goMod string) (string, error) {
	b, err := os.ReadFile(goMod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", goMod)
}

// skipDir reports directories that hold no consumer-facing package.
func skipDir(name string) bool {
	switch name {
	case "testdata", "vendor", "internal", "docs", "scripts", "lint":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func scan(root string) (pkgs []pkg, missing []string, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir // a nested module is not part of this index
			}
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		doc, ok, perr := packageDoc(path)
		if perr != nil {
			return perr
		}
		if !ok {
			return nil // no Go package here, or package main
		}
		if strings.TrimSpace(doc) == "" {
			missing = append(missing, rel)
			return nil
		}
		name := filepath.Base(path)
		pkgs = append(pkgs, pkg{dir: rel, summary: summarize(name, doc), deprecated: isDeprecated(doc)})
		return nil
	})
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].dir < pkgs[j].dir })
	sort.Strings(missing)
	return pkgs, missing, err
}

// packageDoc returns the package comment of the Go package in dir. ok is false
// when dir has no non-test Go files or holds a main package. doc.go wins when
// present; otherwise the first file (by name) that has a comment is used.
func packageDoc(dir string) (doc string, ok bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", false, nil
	}
	sort.Slice(names, func(i, j int) bool { // doc.go first
		if (names[i] == "doc.go") != (names[j] == "doc.go") {
			return names[i] == "doc.go"
		}
		return names[i] < names[j]
	})
	fset := token.NewFileSet()
	for _, n := range names {
		f, perr := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments|parser.PackageClauseOnly)
		if perr != nil {
			return "", false, perr
		}
		if f.Name.Name == "main" {
			return "", false, nil
		}
		ok = true
		if f.Doc != nil && strings.TrimSpace(doc) == "" {
			doc = f.Doc.Text()
		}
	}
	return doc, ok, nil
}

func isDeprecated(doc string) bool {
	for _, para := range strings.Split(doc, "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(para), "Deprecated:") {
			return true
		}
	}
	return false
}

// summarize turns "Package x does a thing. More text." into "Does a thing." It
// takes the first paragraph, drops the "Package x " lead and keeps the first
// sentence, shortened at a word boundary if it is very long.
func summarize(name, doc string) string {
	para := strings.TrimSpace(strings.SplitN(strings.TrimSpace(doc), "\n\n", 2)[0])
	para = strings.Join(strings.Fields(para), " ")
	para = strings.TrimPrefix(para, "Package "+name+" ")
	para = strings.TrimPrefix(para, "Package "+name+": ")
	// "Package x is the canonical client for y" reads as "The canonical client
	// for y"; verbs ("Provides ...") are left alone.
	para = strings.TrimPrefix(para, "is ")
	if end := sentenceEnd(para); end >= 0 {
		para = para[:end+1]
	}
	const limit = 200
	if len(para) > limit {
		cut := strings.LastIndex(para[:limit], " ")
		if cut < 0 {
			cut = limit
		}
		para = strings.TrimRight(para[:cut], ",;:") + "…"
	}
	if para != "" {
		r := []rune(para)
		r[0] = unicode.ToUpper(r[0])
		para = string(r)
	}
	return strings.ReplaceAll(para, "|", `\|`)
}

// sentenceEnd returns the index of the '.' that ends the first sentence: one
// followed by a space and a capital letter, or the final character. "e.g. the"
// does not end a sentence.
func sentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '.' {
			continue
		}
		if i == len(s)-1 {
			return i
		}
		if s[i+1] == ' ' && i+2 < len(s) && unicode.IsUpper(rune(s[i+2])) {
			return i
		}
	}
	return -1
}

func render(module string, pkgs []pkg) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!-- Code generated by `go run ./scripts/pkgindex`; DO NOT EDIT. -->\n\n")
	fmt.Fprintf(&b, "# Package index\n\n")
	fmt.Fprintf(&b, "%d importable packages, built from each package's doc comment. ", len(pkgs))
	fmt.Fprintf(&b, "The import path is `%s/<package>`. ", module)
	fmt.Fprintf(&b, "To change a summary, edit that package's doc comment and regenerate; CI fails if this file is stale.\n\n")
	fmt.Fprintf(&b, "| Package | Summary |\n| --- | --- |\n")
	for _, p := range pkgs {
		note := ""
		if p.deprecated {
			note = " **Deprecated.**"
		}
		fmt.Fprintf(&b, "| [`%s`](../%s/) | %s%s |\n", p.dir, p.dir, p.summary, note)
	}
	return b.Bytes()
}

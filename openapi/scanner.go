package openapi

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScanDir walks dir for *.go files and adds any @openapi-annotated handler
// comments to spec.  Returns the number of routes added.
//
// Comment annotation format (all lines must be consecutive, directly above
// the func declaration or anywhere in the file; mixed-in blank comment lines
// are tolerated):
//
//	// @openapi GET /example
//	// @summary Returns an example response
//	// @tag example
//	// @response 200 {"message":"string"}
//	func handleExample(w http.ResponseWriter, r *http.Request) { … }
//
// Only @openapi, @summary, @tag, and @response are consumed.  Unknown
// @-prefixed lines are silently ignored so the format can be extended
// forwards-compatibly.
func ScanDir(dir string, spec *Spec) (int, error) {
	var added int

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip hidden/vendor directories.
			name := d.Name()
			if name != "." && (name[0] == '.' || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		n, err := scanFile(path, spec)
		if err != nil {
			return err
		}
		added += n
		return nil
	})

	return added, err
}

// scanFile scans a single .go file for @openapi annotations.
func scanFile(path string, spec *Spec) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return scanReader(bufio.NewScanner(f), spec), nil
}

// scanReader is the core parser, broken out so tests can inject a
// strings.NewReader without touching the filesystem.
func scanReader(scanner *bufio.Scanner, spec *Spec) int {
	type pending struct {
		method   string
		path     string
		summary  string
		tags     []string
		response Response // first @response line wins
	}

	var cur *pending
	added := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Only look inside Go comment lines.
		body, isComment := commentBody(line)
		if !isComment {
			// Non-comment line — flush any in-progress annotation block.
			if cur != nil {
				op := Operation{
					Summary:   cur.summary,
					Tags:      cur.tags,
					Responses: make(map[string]Response),
				}
				if cur.response.Description != "" {
					op.Responses["200"] = cur.response
				} else {
					// No @response → default empty 200.
					op.Responses["200"] = Response{Description: "OK"}
				}
				spec.AddRoute(cur.method, cur.path, op)
				added++
				cur = nil
			}
			continue
		}

		// Trim leading/trailing whitespace from the comment body.
		body = strings.TrimSpace(body)

		if strings.HasPrefix(body, "@openapi ") {
			// Start a new block (or replace one if @openapi appears twice
			// in a row, which is a malformed annotation — last one wins).
			parts := strings.Fields(body[len("@openapi "):])
			if len(parts) < 2 {
				continue // malformed — skip
			}
			cur = &pending{
				method: toUpper(parts[0]),
				path:   parts[1],
			}
			continue
		}

		if cur == nil {
			// Annotation directives before an @openapi opener are ignored.
			continue
		}

		switch {
		case strings.HasPrefix(body, "@summary "):
			cur.summary = strings.TrimSpace(body[len("@summary "):])

		case strings.HasPrefix(body, "@tag "):
			tag := strings.TrimSpace(body[len("@tag "):])
			if tag != "" {
				cur.tags = append(cur.tags, tag)
			}

		case strings.HasPrefix(body, "@response "):
			rest := strings.TrimSpace(body[len("@response "):])
			// rest is "<statusCode> <description or JSON example>"
			// We ignore the status code and treat everything after it as
			// the description.  A richer parser can decode the JSON shape
			// into a Schema in the future.
			fields := strings.SplitN(rest, " ", 2)
			desc := ""
			if len(fields) >= 2 {
				desc = fields[1]
			} else if len(fields) == 1 {
				desc = fields[0]
			}
			if cur.response.Description == "" {
				// First @response wins.
				cur.response = Response{Description: desc}
			}
		}
	}

	// Flush if the file ended while inside an annotation block (edge case:
	// annotation at the very end of a file with no trailing newline).
	if cur != nil {
		op := Operation{
			Summary:   cur.summary,
			Tags:      cur.tags,
			Responses: make(map[string]Response),
		}
		if cur.response.Description != "" {
			op.Responses["200"] = cur.response
		} else {
			op.Responses["200"] = Response{Description: "OK"}
		}
		spec.AddRoute(cur.method, cur.path, op)
		added++
	}

	return added
}

// commentBody returns the content after the leading "//" marker, and true,
// when line is a Go line comment.  Otherwise it returns ("", false).
func commentBody(line string) (string, bool) {
	if strings.HasPrefix(line, "//") {
		return line[2:], true
	}
	return "", false
}

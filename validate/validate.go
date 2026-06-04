// Package validate provides struct-tag-driven JSON request decoding and
// validation. It replaces the per-handler if/else chains that every
// fleet service currently hand-rolls.
//
// Supported struct tags (all on the `validate` key):
//
//	required           field must be non-zero
//	min=N              string min length / number minimum value
//	max=N              string max length / number maximum value
//	email              basic email format check
//	url                basic URL format check (must start with http:// or https://)
//	oneof=a|b|c        value must equal one of the pipe-separated options
//	pattern=<regexp>   string must match the regexp (anchored)
//
// Usage:
//
//	type CreateReq struct {
//	    Name  string `json:"name"  validate:"required,max=64"`
//	    Email string `json:"email" validate:"required,email"`
//	    Role  string `json:"role"  validate:"oneof=admin|user|viewer"`
//	}
//	var req CreateReq
//	if err := validate.Bind(r, &req); err != nil {
//	    // err is *errors.Error with Status=400 and Code="bad_request.validation"
//	    http.Error(w, err.Error(), err.HTTPStatus())
//	    return
//	}
//
// Bind decodes JSON, then validates. Validation errors are aggregated
// into a single *errors.Error so the handler sees all failures at once.
package validate

import (
	"encoding/json"
	"fmt"
	fleetErrors "github.com/baditaflorin/go-common/errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
)

// Struct validates the `validate` struct tags on dst without decoding.
// Returns a *fleetErrors.Error aggregating all field failures.
func Struct(dst any) *fleetErrors.Error {
	errs := validateValue(reflect.ValueOf(dst), "")
	if len(errs) == 0 {
		return nil
	}
	return fleetErrors.New(http.StatusBadRequest, "bad_request.validation",
		strings.Join(errs, "; "))
}

// ─── internals ────────────────────────────────────────────────────────────

func decodeJSON(r *http.Request, dst any) error {
	body := r.Body
	if body == nil {
		return fmt.Errorf("request body is empty")
	}
	defer body.Close()
	dec := json.NewDecoder(io.LimitReader(body, 4<<20)) // 4 MiB max
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("JSON decode: %w", err)
	}
	return nil
}

// regexpCache caches compiled regexps to avoid re-compiling on each request.
var regexpCache = struct {
	mu interface {
		Lock()
		Unlock()
	}
	items map[string]*regexp.Regexp
}{items: make(map[string]*regexp.Regexp)}

// simple mutex via sync.Mutex embedded in a struct
var reCache = newRegexpCache()

type regCache struct {
	mu interface {
		Lock()
		Unlock()
	}
	items map[string]*regexp.Regexp
}

func newRegexpCache() *safeRegexpCache {
	c := &safeRegexpCache{
		items: make(map[string]*regexp.Regexp),
		sem:   make(chan struct{}, 1),
	}
	c.sem <- struct{}{}
	return c
}

func validateValue(v reflect.Value, prefix string) []string {
	// Dereference pointer.
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()

	var errs []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)
		tag := field.Tag.Get("validate")
		if tag == "" || tag == "-" {
			// Recurse into nested structs even without validate tags.
			if fv.Kind() == reflect.Struct ||
				(fv.Kind() == reflect.Ptr && !fv.IsNil() && fv.Elem().Kind() == reflect.Struct) {
				errs = append(errs, validateValue(fv, fieldName(prefix, field))...)
			}
			continue
		}
		name := fieldName(prefix, field)
		errs = append(errs, applyRules(fv, name, tag)...)
		// Recurse into embedded/nested structs.
		errs = append(errs, validateValue(fv, name)...)
	}
	return errs
}

func fieldName(prefix string, f reflect.StructField) string {
	name := f.Tag.Get("json")
	if name == "" || name == "-" {
		name = f.Name
	} else {
		name = strings.Split(name, ",")[0]
	}
	if prefix != "" {
		return prefix + "." + name
	}
	return name
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.String() == ""
	case reflect.Slice, reflect.Map:
		return v.IsNil() || v.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Bool:
		return !v.Bool()
	}
	return false
}

func stringify(v reflect.Value) string {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func isEmail(s string) bool { return emailRe.MatchString(s) }

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

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
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	fleetErrors "github.com/baditaflorin/go-common/errors"
)

// Bind decodes the JSON body of r into dst (which must be a non-nil
// pointer to a struct), then validates all `validate` struct tags.
// Returns a *fleetErrors.Error with Status=400 on any decode or
// validation failure. Returns nil on success.
func Bind(r *http.Request, dst any) *fleetErrors.Error {
	if err := decodeJSON(r, dst); err != nil {
		return fleetErrors.New(http.StatusBadRequest, "bad_request.json", err.Error())
	}
	return Struct(dst)
}

// BindBytes decodes b (raw JSON) into dst, then validates.
func BindBytes(b []byte, dst any) *fleetErrors.Error {
	if err := json.Unmarshal(b, dst); err != nil {
		return fleetErrors.New(http.StatusBadRequest, "bad_request.json",
			"invalid JSON: "+err.Error())
	}
	return Struct(dst)
}

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
	mu    interface{ Lock(); Unlock() }
	items map[string]*regexp.Regexp
}{items: make(map[string]*regexp.Regexp)}

// simple mutex via sync.Mutex embedded in a struct
var reCache = newRegexpCache()

type regCache struct {
	mu    interface{ Lock(); Unlock() }
	items map[string]*regexp.Regexp
}

// We use a plain map protected by a channel semaphore to avoid
// importing sync explicitly (it's in stdlib but let's be explicit).
type safeRegexpCache struct {
	items map[string]*regexp.Regexp
	sem   chan struct{}
}

func newRegexpCache() *safeRegexpCache {
	c := &safeRegexpCache{
		items: make(map[string]*regexp.Regexp),
		sem:   make(chan struct{}, 1),
	}
	c.sem <- struct{}{}
	return c
}

func (c *safeRegexpCache) compile(pattern string) (*regexp.Regexp, error) {
	<-c.sem
	defer func() { c.sem <- struct{}{} }()
	if re, ok := c.items[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	c.items[pattern] = re
	return re, nil
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

func applyRules(v reflect.Value, name, tag string) []string {
	rules := strings.Split(tag, ",")
	var errs []string
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if err := applyRule(v, name, rule); err != "" {
			errs = append(errs, err)
		}
	}
	return errs
}

func applyRule(v reflect.Value, name, rule string) string {
	switch {
	case rule == "required":
		if isZero(v) {
			return fmt.Sprintf("%s: required", name)
		}

	case strings.HasPrefix(rule, "min="):
		n, err := strconv.ParseFloat(rule[4:], 64)
		if err != nil {
			return fmt.Sprintf("%s: invalid min tag", name)
		}
		if msg := checkMin(v, name, n); msg != "" {
			return msg
		}

	case strings.HasPrefix(rule, "max="):
		n, err := strconv.ParseFloat(rule[4:], 64)
		if err != nil {
			return fmt.Sprintf("%s: invalid max tag", name)
		}
		if msg := checkMax(v, name, n); msg != "" {
			return msg
		}

	case rule == "email":
		s := stringify(v)
		if s != "" && !isEmail(s) {
			return fmt.Sprintf("%s: must be a valid email address", name)
		}

	case rule == "url":
		s := stringify(v)
		if s != "" && !isURL(s) {
			return fmt.Sprintf("%s: must be a valid URL (http:// or https://)", name)
		}

	case strings.HasPrefix(rule, "oneof="):
		choices := strings.Split(rule[6:], "|")
		s := stringify(v)
		if !contains(choices, s) {
			return fmt.Sprintf("%s: must be one of [%s]", name, strings.Join(choices, ", "))
		}

	case strings.HasPrefix(rule, "pattern="):
		pattern := rule[8:]
		s := stringify(v)
		if s == "" {
			break
		}
		re, err := reCache.compile(pattern)
		if err != nil {
			return fmt.Sprintf("%s: invalid pattern %q in tag", name, pattern)
		}
		if !re.MatchString(s) {
			return fmt.Sprintf("%s: does not match pattern %q", name, pattern)
		}
	}
	return ""
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

func checkMin(v reflect.Value, name string, n float64) string {
	switch v.Kind() {
	case reflect.String:
		if float64(len(v.String())) < n {
			return fmt.Sprintf("%s: must be at least %g characters", name, n)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if float64(v.Int()) < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if float64(v.Uint()) < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() < n {
			return fmt.Sprintf("%s: must be at least %g", name, n)
		}
	case reflect.Slice, reflect.Map:
		if float64(v.Len()) < n {
			return fmt.Sprintf("%s: must have at least %g items", name, n)
		}
	}
	return ""
}

func checkMax(v reflect.Value, name string, n float64) string {
	switch v.Kind() {
	case reflect.String:
		if float64(len(v.String())) > n {
			return fmt.Sprintf("%s: must be at most %g characters", name, n)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if float64(v.Int()) > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if float64(v.Uint()) > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Float32, reflect.Float64:
		if v.Float() > n {
			return fmt.Sprintf("%s: must be at most %g", name, n)
		}
	case reflect.Slice, reflect.Map:
		if float64(v.Len()) > n {
			return fmt.Sprintf("%s: must have at most %g items", name, n)
		}
	}
	return ""
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

package validate

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

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
		// The tag is documented as anchored, so the whole value must match. A bare
		// MatchString only needs a substring: "[A-Z]{2}[0-9]{3}" would accept
		// "AA0000" and "<script>AA123". Wrapping is harmless for a pattern that
		// already carries ^ and $, and keeps an alternation (a|b) from matching
		// a prefix; (?i) and other inline flags stay scoped to the group.
		re, err := reCache.compile("^(?:" + pattern + ")$")
		if err != nil {
			return fmt.Sprintf("%s: invalid pattern %q in tag", name, pattern)
		}
		if !re.MatchString(s) {
			return fmt.Sprintf("%s: does not match pattern %q", name, pattern)
		}
	}
	return ""
}

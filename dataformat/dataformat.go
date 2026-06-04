package dataformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	yaml "go.yaml.in/yaml/v2"
	"sort"
	"strconv"
	"strings"
)

const (
	// JSON is RFC 8259 JSON.
	JSON Format = iota
	// CSV is RFC 4180 comma-separated values with a header row.
	CSV
	// XML is XML 1.0.
	XML
	// YAML is YAML 1.1 (via go.yaml.in/yaml/v2).
	YAML
	// TOML is TOML v1.0.0 (via github.com/BurntSushi/toml).
	TOML
)

// Sentinel errors. Use errors.Is to test for them.
var (
	// ErrUnknownFormat is returned by ParseFormat for an unrecognized name.
	ErrUnknownFormat = errors.New("dataformat: unknown format")
	// ErrDetectFailed is returned by DetectFormat when no format matches.
	ErrDetectFailed = errors.New("dataformat: could not detect format")
	// ErrNotTabular is returned when a value cannot be shaped into CSV rows.
	ErrNotTabular = errors.New("dataformat: value is not tabular")
	// ErrUnsupportedShape is returned when a value cannot be represented in
	// the target format (e.g. a bare array/scalar to TOML or XML).
	ErrUnsupportedShape = errors.New("dataformat: value shape unsupported by target format")
	// ErrEmptyInput is returned when decoding empty input.
	ErrEmptyInput = errors.New("dataformat: empty input")
)

// ParseFormat maps a (case-insensitive) name to a Format. It accepts the
// canonical names plus a few common aliases ("yml", "text/csv", ...).
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json", "application/json":
		return JSON, nil
	case "csv", "text/csv":
		return CSV, nil
	case "xml", "application/xml", "text/xml":
		return XML, nil
	case "yaml", "yml", "application/yaml", "text/yaml":
		return YAML, nil
	case "toml", "application/toml":
		return TOML, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownFormat, s)
	}
}

// DetectFormat makes a best-effort guess at the format of b. It is a
// heuristic sniffer, not a validator: the result is the most plausible
// format, and a follow-up Decode is the authoritative check. Detection
// order is chosen so that the most structurally distinctive formats win.
func DetectFormat(b []byte) (Format, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return 0, ErrEmptyInput
	}

	// XML: starts with a declaration or an element tag.
	if trimmed[0] == '<' {
		return XML, nil
	}

	// JSON: object or array literal, and it must actually parse.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var js any
		if json.Unmarshal(trimmed, &js) == nil {
			return JSON, nil
		}
	}

	// TOML: a line that looks like a [table] header or key = value, and it
	// parses as TOML. Checked before YAML because valid TOML is often also
	// accepted by the permissive YAML parser, but not vice-versa.
	if looksLikeTOML(trimmed) {
		var tm map[string]any
		if _, err := toml.Decode(string(trimmed), &tm); err == nil {
			return TOML, nil
		}
	}

	// CSV: at least two comma-separated columns on the first line and the
	// reader accepts a consistent record shape.
	if looksLikeCSV(trimmed) {
		return CSV, nil
	}

	// YAML: last resort for the remaining "key: value" shape. It must parse
	// into a map or slice (a bare scalar string is too ambiguous to claim).
	var yv any
	if yaml.Unmarshal(trimmed, &yv) == nil {
		switch yv.(type) {
		case map[any]any, map[string]any, []any:
			return YAML, nil
		}
	}

	return 0, fmt.Errorf("%w", ErrDetectFailed)
}

// Decode parses b in format f into a generic Go value composed of
// map[string]any, []any and scalars, suitable for re-encoding into any
// other format. Parse failures are returned as *DecodeError.
func Decode(f Format, b []byte) (any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, &DecodeError{Format: f, Err: ErrEmptyInput}
	}
	switch f {
	case JSON:
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, &DecodeError{Format: f, Err: err}
		}
		return v, nil

	case YAML:
		var v any
		if err := yaml.Unmarshal(b, &v); err != nil {
			return nil, &DecodeError{Format: f, Err: err}
		}
		return normalizeYAML(v), nil

	case TOML:
		var v map[string]any
		if _, err := toml.Decode(string(b), &v); err != nil {
			return nil, &DecodeError{Format: f, Err: err}
		}
		return v, nil

	case CSV:
		v, err := decodeCSV(b)
		if err != nil {
			return nil, &DecodeError{Format: f, Err: err}
		}
		return v, nil

	case XML:
		v, err := decodeXML(b)
		if err != nil {
			return nil, &DecodeError{Format: f, Err: err}
		}
		return v, nil

	default:
		return nil, &DecodeError{Format: f, Err: fmt.Errorf("%w: %s", ErrUnknownFormat, f)}
	}
}

// Encode serializes v into format f. Shape mismatches (e.g. a bare array to
// TOML) are returned as *EncodeError wrapping ErrUnsupportedShape or
// ErrNotTabular.
func Encode(f Format, v any) ([]byte, error) {
	switch f {
	case JSON:
		out, err := json.Marshal(v)
		if err != nil {
			return nil, &EncodeError{Format: f, Err: err}
		}
		return out, nil

	case YAML:
		out, err := yaml.Marshal(v)
		if err != nil {
			return nil, &EncodeError{Format: f, Err: err}
		}
		return out, nil

	case TOML:
		m, ok := asStringMap(v)
		if !ok {
			return nil, &EncodeError{Format: f, Err: fmt.Errorf("%w: TOML requires a top-level table", ErrUnsupportedShape)}
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(m); err != nil {
			return nil, &EncodeError{Format: f, Err: err}
		}
		return buf.Bytes(), nil

	case CSV:
		out, err := encodeCSV(v)
		if err != nil {
			return nil, &EncodeError{Format: f, Err: err}
		}
		return out, nil

	case XML:
		out, err := encodeXML(v)
		if err != nil {
			return nil, &EncodeError{Format: f, Err: err}
		}
		return out, nil

	default:
		return nil, &EncodeError{Format: f, Err: fmt.Errorf("%w: %s", ErrUnknownFormat, f)}
	}
}

// Convert decodes b from the "from" format and re-encodes it as "to". It is
// a thin composition of Decode and Encode; lossy edges (see package docs)
// apply to the encode step.
func Convert(from, to Format, b []byte) ([]byte, error) {
	v, err := Decode(from, b)
	if err != nil {
		return nil, err
	}
	return Encode(to, v)
}

// --- CSV ---------------------------------------------------------------

// --- XML ---------------------------------------------------------------

func nodeToValue(n xmlNode) any {
	obj := map[string]any{}
	for _, a := range n.Attrs {
		obj["-"+a.Name.Local] = a.Value
	}
	// Group children by name to detect repeats.
	grouped := map[string][]any{}
	order := []string{}
	for _, c := range n.Children {
		name := c.XMLName.Local
		if _, seen := grouped[name]; !seen {
			order = append(order, name)
		}
		grouped[name] = append(grouped[name], nodeToValue(c))
	}
	for _, name := range order {
		vals := grouped[name]
		if len(vals) == 1 {
			obj[name] = vals[0]
		} else {
			obj[name] = toAnySlice(vals)
		}
	}
	text := strings.TrimSpace(n.Content)
	if len(obj) == 0 {
		// Leaf element: represent as its text content.
		return text
	}
	if text != "" {
		obj["#text"] = text
	}
	return obj
}

type kv struct {
	k string
	v any
}

func splitXMLFields(m map[string]any) (attrs []kv, children []kv, text string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case k == "#text":
			text = scalarToString(m[k])
		case strings.HasPrefix(k, "-"):
			attrs = append(attrs, kv{k[1:], scalarToString(m[k])})
		default:
			children = append(children, kv{k, m[k]})
		}
	}
	return attrs, children, text
}

// --- helpers -----------------------------------------------------------

// normalizeYAML rewrites map[any]any (produced by yaml/v2) into
// map[string]any recursively, so the value matches the JSON-style generic
// model used everywhere else.
func normalizeYAML(v any) any {
	switch val := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[fmt.Sprintf("%v", k)] = normalizeYAML(vv)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = normalizeYAML(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = normalizeYAML(vv)
		}
		return out
	default:
		return v
	}
}

// asStringMap coerces a value to map[string]any, accepting the yaml/v2
// map[any]any shape as well.
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, vv := range m {
			out[fmt.Sprintf("%v", k)] = vv
		}
		return out, true
	default:
		return nil, false
	}
}

func toAnySlice(in []any) []any { return in }

// scalarToString renders a scalar for tabular/text output. Maps and slices
// fall back to their JSON encoding so a cell is never empty-by-accident.
func scalarToString(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

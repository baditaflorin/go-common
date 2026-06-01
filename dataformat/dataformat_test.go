package dataformat

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// jsonEqual compares two byte slices as semantically-equal JSON documents,
// ignoring key order and formatting.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("left is not valid JSON: %v (%s)", err, a)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("right is not valid JSON: %v (%s)", err, b)
	}
	return reflect.DeepEqual(av, bv)
}

func TestParseFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"json", JSON, false},
		{"JSON", JSON, false},
		{" application/json ", JSON, false},
		{"csv", CSV, false},
		{"text/csv", CSV, false},
		{"xml", XML, false},
		{"text/xml", XML, false},
		{"yaml", YAML, false},
		{"yml", YAML, false},
		{"toml", TOML, false},
		{"application/toml", TOML, false},
		{"protobuf", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseFormat(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.in)
				}
				if !errors.Is(err, ErrUnknownFormat) {
					t.Fatalf("expected ErrUnknownFormat, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("ParseFormat(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestFormatString(t *testing.T) {
	cases := []struct {
		f    Format
		want string
	}{
		{JSON, "json"},
		{CSV, "csv"},
		{XML, "xml"},
		{YAML, "yaml"},
		{TOML, "toml"},
		{Format(99), "Format(99)"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.f.String(); got != c.want {
				t.Fatalf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Format
		wantErr error
	}{
		{"json-object", `{"a":1,"b":"x"}`, JSON, nil},
		{"json-array", `[1,2,3]`, JSON, nil},
		{"xml-decl", `<?xml version="1.0"?><root><a>1</a></root>`, XML, nil},
		{"xml-bare", `<root><a>1</a></root>`, XML, nil},
		{"toml-table", "[server]\nhost = \"x\"\nport = 80\n", TOML, nil},
		{"toml-keyval", "name = \"florin\"\nage = 30\n", TOML, nil},
		{"csv", "name,age\nflorin,30\nalice,28\n", CSV, nil},
		{"yaml-map", "name: florin\nage: 30\n", YAML, nil},
		{"yaml-list", "- a\n- b\n- c\n", YAML, nil},
		{"empty", "   \n  ", 0, ErrEmptyInput},
		{"garbage", "\x00\x01\x02not anything", 0, ErrDetectFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DetectFormat([]byte(c.in))
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("DetectFormat error = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("DetectFormat(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRoundTripJSONYAMLJSON(t *testing.T) {
	cases := []string{
		`{"name":"florin","age":30,"tags":["a","b"],"nested":{"k":true}}`,
		`[1,2,3,4]`,
		`{"empty":{},"list":[],"null":null}`,
		`{"float":3.14,"bool":false,"str":"hello world"}`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			// JSON -> YAML
			yamlBytes, err := Convert(JSON, YAML, []byte(src))
			if err != nil {
				t.Fatalf("JSON->YAML: %v", err)
			}
			// YAML -> JSON
			jsonBytes, err := Convert(YAML, JSON, yamlBytes)
			if err != nil {
				t.Fatalf("YAML->JSON: %v", err)
			}
			if !jsonEqual(t, []byte(src), jsonBytes) {
				t.Fatalf("round-trip mismatch:\n  src = %s\n  got = %s\n  via = %s", src, jsonBytes, yamlBytes)
			}
		})
	}
}

func TestCSVToJSONHeaderInference(t *testing.T) {
	csvIn := "name,age,city\nflorin,30,bucharest\nalice,28,berlin\n"
	out, err := Convert(CSV, JSON, []byte(csvIn))
	if err != nil {
		t.Fatalf("CSV->JSON: %v", err)
	}
	want := `[{"name":"florin","age":"30","city":"bucharest"},{"name":"alice","age":"28","city":"berlin"}]`
	if !jsonEqual(t, []byte(want), out) {
		t.Fatalf("CSV->JSON mismatch:\n  got = %s\n  want = %s", out, want)
	}
}

func TestJSONToCSVArrayOfObjects(t *testing.T) {
	jsonIn := `[{"name":"florin","age":30},{"name":"alice","age":28}]`
	out, err := Convert(JSON, CSV, []byte(jsonIn))
	if err != nil {
		t.Fatalf("JSON->CSV: %v", err)
	}
	got := strings.TrimSpace(string(out))
	// keys are unioned + sorted -> "age,name"
	want := "age,name\n30,florin\n28,alice"
	if got != want {
		t.Fatalf("JSON->CSV mismatch:\n  got  = %q\n  want = %q", got, want)
	}
}

func TestJSONToCSVUnionedKeys(t *testing.T) {
	jsonIn := `[{"a":"1"},{"a":"2","b":"3"}]`
	out, err := Convert(JSON, CSV, []byte(jsonIn))
	if err != nil {
		t.Fatalf("JSON->CSV: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := "a,b\n1,\n2,3"
	if got != want {
		t.Fatalf("JSON->CSV unioned mismatch:\n  got  = %q\n  want = %q", got, want)
	}
}

func TestJSONToCSVNotTabular(t *testing.T) {
	cases := []string{
		`{"a":1}`,            // object, not array
		`[1,2,3]`,            // array of scalars
		`[{"a":1},"string"]`, // mixed
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Convert(JSON, CSV, []byte(src))
			if !errors.Is(err, ErrNotTabular) {
				t.Fatalf("expected ErrNotTabular, got %v", err)
			}
			var ee *EncodeError
			if !errors.As(err, &ee) {
				t.Fatalf("expected *EncodeError, got %T", err)
			}
		})
	}
}

func TestTOMLToJSON(t *testing.T) {
	tomlIn := "name = \"florin\"\nage = 30\n\n[server]\nhost = \"localhost\"\nport = 8080\n"
	out, err := Convert(TOML, JSON, []byte(tomlIn))
	if err != nil {
		t.Fatalf("TOML->JSON: %v", err)
	}
	want := `{"name":"florin","age":30,"server":{"host":"localhost","port":8080}}`
	if !jsonEqual(t, []byte(want), out) {
		t.Fatalf("TOML->JSON mismatch:\n  got  = %s\n  want = %s", out, want)
	}
}

func TestJSONToTOMLRoundTrip(t *testing.T) {
	jsonIn := `{"name":"florin","server":{"host":"localhost","port":8080}}`
	tomlBytes, err := Convert(JSON, TOML, []byte(jsonIn))
	if err != nil {
		t.Fatalf("JSON->TOML: %v", err)
	}
	back, err := Convert(TOML, JSON, tomlBytes)
	if err != nil {
		t.Fatalf("TOML->JSON: %v", err)
	}
	if !jsonEqual(t, []byte(jsonIn), back) {
		t.Fatalf("JSON->TOML->JSON mismatch:\n  got = %s\n  via = %s", back, tomlBytes)
	}
}

func TestTOMLEncodeUnsupportedShape(t *testing.T) {
	// A bare array cannot be a TOML document.
	_, err := Convert(JSON, TOML, []byte(`[1,2,3]`))
	if !errors.Is(err, ErrUnsupportedShape) {
		t.Fatalf("expected ErrUnsupportedShape, got %v", err)
	}
}

func TestXMLDecodeAttributesAndText(t *testing.T) {
	xmlIn := `<book id="42"><title>Go</title><author>Florin</author></book>`
	v, err := Decode(XML, []byte(xmlIn))
	if err != nil {
		t.Fatalf("decode XML: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map root, got %T", v)
	}
	book, ok := m["book"].(map[string]any)
	if !ok {
		t.Fatalf("expected book map, got %T", m["book"])
	}
	if book["-id"] != "42" {
		t.Fatalf("attribute id = %v, want 42", book["-id"])
	}
	if book["title"] != "Go" {
		t.Fatalf("title = %v, want Go", book["title"])
	}
}

func TestXMLRepeatedChildrenBecomeArray(t *testing.T) {
	xmlIn := `<list><item>a</item><item>b</item><item>c</item></list>`
	v, err := Decode(XML, []byte(xmlIn))
	if err != nil {
		t.Fatalf("decode XML: %v", err)
	}
	m := v.(map[string]any)
	list := m["list"].(map[string]any)
	items, ok := list["item"].([]any)
	if !ok {
		t.Fatalf("expected item array, got %T", list["item"])
	}
	if len(items) != 3 || items[0] != "a" || items[2] != "c" {
		t.Fatalf("unexpected items: %v", items)
	}
}

func TestXMLRoundTrip(t *testing.T) {
	xmlIn := `<config><host>localhost</host><port>8080</port></config>`
	// XML -> JSON -> XML
	jsonBytes, err := Convert(XML, JSON, []byte(xmlIn))
	if err != nil {
		t.Fatalf("XML->JSON: %v", err)
	}
	xmlBytes, err := Convert(JSON, XML, jsonBytes)
	if err != nil {
		t.Fatalf("JSON->XML: %v", err)
	}
	// Decode both and compare structurally.
	a, _ := Decode(XML, []byte(xmlIn))
	b, _ := Decode(XML, xmlBytes)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("XML round-trip mismatch:\n  a = %v\n  b = %v\n  xml = %s", a, b, xmlBytes)
	}
}

func TestXMLEncodeUnsupportedShape(t *testing.T) {
	cases := []string{
		`[1,2,3]`,                 // top-level array
		`{"a":1,"b":2}`,           // multiple roots
		`"just a string"`,         // bare scalar
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			_, err := Convert(JSON, XML, []byte(src))
			if !errors.Is(err, ErrUnsupportedShape) {
				t.Fatalf("expected ErrUnsupportedShape, got %v", err)
			}
		})
	}
}

func TestDecodeMalformed(t *testing.T) {
	cases := []struct {
		name string
		f    Format
		in   string
	}{
		{"json", JSON, `{"a": }`},
		{"yaml", YAML, "key: value:\n  - bad\n :::"},
		{"toml", TOML, "key = = bad"},
		{"xml", XML, `<root><unclosed></root>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode(c.f, []byte(c.in))
			if err == nil {
				t.Fatalf("expected decode error for malformed %s", c.f)
			}
			var de *DecodeError
			if !errors.As(err, &de) {
				t.Fatalf("expected *DecodeError, got %T: %v", err, err)
			}
			if de.Format != c.f {
				t.Fatalf("DecodeError.Format = %v, want %v", de.Format, c.f)
			}
		})
	}
}

func TestDecodeEmpty(t *testing.T) {
	for _, f := range []Format{JSON, CSV, XML, YAML, TOML} {
		t.Run(f.String(), func(t *testing.T) {
			_, err := Decode(f, []byte("   "))
			if !errors.Is(err, ErrEmptyInput) {
				t.Fatalf("expected ErrEmptyInput, got %v", err)
			}
		})
	}
}

func TestDecodeSampleEachFormat(t *testing.T) {
	cases := []struct {
		f  Format
		in string
	}{
		{JSON, `{"ok":true}`},
		{YAML, "ok: true\n"},
		{TOML, "ok = true\n"},
		{XML, `<root><ok>true</ok></root>`},
		{CSV, "ok\ntrue\n"},
	}
	for _, c := range cases {
		t.Run(c.f.String(), func(t *testing.T) {
			v, err := Decode(c.f, []byte(c.in))
			if err != nil {
				t.Fatalf("decode %s: %v", c.f, err)
			}
			if v == nil {
				t.Fatalf("decode %s returned nil", c.f)
			}
		})
	}
}

func TestEncodeUnknownFormat(t *testing.T) {
	_, err := Encode(Format(99), map[string]any{"a": 1})
	if !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("expected ErrUnknownFormat, got %v", err)
	}
}

func TestConvertAllPairs(t *testing.T) {
	// A value that is faithfully representable in every format: a single
	// object whose values are all strings. CSV needs an array, so the CSV
	// legs are handled separately below; here we exercise the map-friendly
	// formats across all pairs.
	mapFormats := []Format{JSON, YAML, TOML, XML}
	srcJSON := `{"root":{"host":"localhost","port":"8080"}}`
	// For non-XML targets, a flat object works; XML needs single root, which
	// the wrapper "root" provides.

	for _, from := range mapFormats {
		// Seed the "from" representation by converting from JSON.
		seed, err := Convert(JSON, from, []byte(srcJSON))
		if err != nil {
			// JSON->XML of a single-root object should succeed; others too.
			t.Fatalf("seed JSON->%s: %v", from, err)
		}
		for _, to := range mapFormats {
			t.Run(from.String()+"->"+to.String(), func(t *testing.T) {
				out, err := Convert(from, to, seed)
				if err != nil {
					t.Fatalf("Convert %s->%s: %v", from, to, err)
				}
				if len(out) == 0 {
					t.Fatalf("Convert %s->%s produced empty output", from, to)
				}
				// Convert back to JSON and check the inner data survives.
				backJSON, err := Convert(to, JSON, out)
				if err != nil {
					t.Fatalf("Convert %s->json: %v", to, err)
				}
				if !jsonEqual(t, []byte(srcJSON), backJSON) {
					t.Fatalf("data lost on %s->%s->json:\n  got = %s", from, to, backJSON)
				}
			})
		}
	}
}

func TestConvertCSVPairs(t *testing.T) {
	// CSV <-> JSON and CSV <-> YAML (the tabular-friendly legs).
	csvIn := "name,role\nflorin,admin\nalice,dev\n"
	rowsWant := `[{"name":"florin","role":"admin"},{"name":"alice","role":"dev"}]`

	t.Run("csv->json", func(t *testing.T) {
		out, err := Convert(CSV, JSON, []byte(csvIn))
		if err != nil {
			t.Fatalf("CSV->JSON: %v", err)
		}
		if !jsonEqual(t, []byte(rowsWant), out) {
			t.Fatalf("CSV->JSON mismatch: got %s", out)
		}
	})

	t.Run("json->csv->json", func(t *testing.T) {
		csvOut, err := Convert(JSON, CSV, []byte(rowsWant))
		if err != nil {
			t.Fatalf("JSON->CSV: %v", err)
		}
		back, err := Convert(CSV, JSON, csvOut)
		if err != nil {
			t.Fatalf("CSV->JSON: %v", err)
		}
		if !jsonEqual(t, []byte(rowsWant), back) {
			t.Fatalf("CSV round-trip mismatch: got %s via %s", back, csvOut)
		}
	})

	t.Run("csv->yaml->csv", func(t *testing.T) {
		yamlOut, err := Convert(CSV, YAML, []byte(csvIn))
		if err != nil {
			t.Fatalf("CSV->YAML: %v", err)
		}
		csvOut, err := Convert(YAML, CSV, yamlOut)
		if err != nil {
			t.Fatalf("YAML->CSV: %v", err)
		}
		back, err := Convert(CSV, JSON, csvOut)
		if err != nil {
			t.Fatalf("CSV->JSON: %v", err)
		}
		if !jsonEqual(t, []byte(rowsWant), back) {
			t.Fatalf("CSV->YAML->CSV mismatch: got %s", back)
		}
	})
}

func TestConvertCSVRawRows(t *testing.T) {
	// Array-of-arrays encodes as raw CSV rows.
	out, err := Encode(CSV, []any{
		[]any{"a", "b"},
		[]any{"1", "2"},
	})
	if err != nil {
		t.Fatalf("encode CSV raw rows: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := "a,b\n1,2"
	if got != want {
		t.Fatalf("raw rows mismatch: got %q want %q", got, want)
	}
}

func TestYAMLToTOML(t *testing.T) {
	yamlIn := "name: florin\nserver:\n  host: localhost\n  port: 8080\n"
	out, err := Convert(YAML, TOML, []byte(yamlIn))
	if err != nil {
		t.Fatalf("YAML->TOML: %v", err)
	}
	back, err := Convert(TOML, JSON, out)
	if err != nil {
		t.Fatalf("TOML->JSON: %v", err)
	}
	want := `{"name":"florin","server":{"host":"localhost","port":8080}}`
	if !jsonEqual(t, []byte(want), back) {
		t.Fatalf("YAML->TOML mismatch: got %s via %s", back, out)
	}
}

func TestConvertPropagatesDecodeError(t *testing.T) {
	_, err := Convert(JSON, YAML, []byte(`{bad`))
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("expected *DecodeError from Convert, got %T: %v", err, err)
	}
}

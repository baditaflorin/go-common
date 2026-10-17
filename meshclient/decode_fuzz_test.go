package meshclient

import (
	"encoding/json"
	"testing"
)

// FuzzDecode feeds arbitrary bytes to the response decoder, which reads what a
// sibling sends. It must not panic, and it may only accept exactly one valid JSON
// value: the check for trailing data is what stops a second value from slipping in.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `{"a":1} {"a":2}`, ``, `   `, `[1,2]`, `{"a":`, `null`, "{\"a\":1}\n\n", `{"a":1}x`, `"str"`} {
		f.Add([]byte(s))
	}
	c := &Client{name: "fuzz"}
	f.Fuzz(func(t *testing.T, data []byte) {
		var out any
		if err := c.decode(data, &out); err == nil && !json.Valid(data) {
			t.Fatalf("decode accepted input that is not one valid JSON value: %q", data)
		}
	})
}

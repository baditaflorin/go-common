package credentiallease

import (
	"bytes"
	"encoding/json"
	"strings"
)

// responseCleanupStrings reads only the named strings from the first response
// object, for best-effort cleanup after strict decoding failed. Repeated names
// (including case aliases encoding/json accepts) are ambiguous and return empty.
// It never decodes the credential field. Trailing data cannot change the first
// object's identity, matching the existing malformed-response cleanup policy.
func responseCleanupStrings(raw []byte, names ...string) []string {
	values := make([]string, len(names))
	seen := make([]bool, len(names))
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return values
	}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			clear(values)
			return values
		}
		key, ok := token.(string)
		if !ok {
			clear(values)
			return values
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			zero(value)
			clear(values)
			return values
		}
		for i, name := range names {
			if strings.EqualFold(key, name) {
				if seen[i] {
					values[i] = ""
				} else {
					_ = json.Unmarshal(value, &values[i])
					seen[i] = true
				}
			}
		}
		zero(value)
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		clear(values)
	}
	return values
}

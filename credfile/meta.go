package credfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MetaSchema is the sidecar format version this package writes.
const MetaSchema = 1

// Meta is the non-secret sidecar next to a key file ("<path>.meta"). Every
// field is optional; a zero time means unknown (for ExpiresAt: never, or not
// said). It never holds the key: KeySHA256 is a one-way digest that lets a
// reader tell whether the sidecar describes the key in the file.
type Meta struct {
	Schema      int       `json:"schema"`
	KeyID       string    `json:"key_id,omitempty"`
	ExpiresAt   time.Time `json:"-"`
	IssuedAt    time.Time `json:"-"`
	RenewAfter  time.Time `json:"-"`
	SuccessorOf string    `json:"successor_of,omitempty"`
	KeySHA256   string    `json:"key_sha256,omitempty"`
}

type metaWire struct {
	Schema      int             `json:"schema"`
	KeyID       string          `json:"key_id,omitempty"`
	ExpiresAt   json.RawMessage `json:"expires_at,omitempty"`
	IssuedAt    json.RawMessage `json:"issued_at,omitempty"`
	RenewAfter  json.RawMessage `json:"renew_after,omitempty"`
	SuccessorOf string          `json:"successor_of,omitempty"`
	KeySHA256   string          `json:"key_sha256,omitempty"`
}

// MarshalJSON writes the canonical form: times as RFC 3339 in UTC, zero
// times left out, schema set to MetaSchema when it is zero.
func (m Meta) MarshalJSON() ([]byte, error) {
	w := metaWire{Schema: m.Schema, KeyID: m.KeyID, SuccessorOf: m.SuccessorOf, KeySHA256: strings.ToLower(m.KeySHA256)}
	if w.Schema == 0 {
		w.Schema = MetaSchema
	}
	for _, f := range []struct {
		t   time.Time
		dst *json.RawMessage
	}{{m.ExpiresAt, &w.ExpiresAt}, {m.IssuedAt, &w.IssuedAt}, {m.RenewAfter, &w.RenewAfter}} {
		if !f.t.IsZero() {
			*f.dst = json.RawMessage(strconv.Quote(f.t.UTC().Format(time.RFC3339)))
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON is ParseMeta.
func (m *Meta) UnmarshalJSON(data []byte) error {
	parsed, err := ParseMeta(data)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// ParseMeta parses a sidecar. Times may be RFC 3339 strings or unix seconds;
// null, an empty string and 0 mean unknown. Unknown fields are ignored, so a
// newer writer can add some. key_sha256, when present, must be 64 hex digits.
func ParseMeta(data []byte) (Meta, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return Meta{}, errors.New("credfile: meta is not a JSON object")
	}
	var w metaWire
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&w); err != nil {
		return Meta{}, fmt.Errorf("credfile: meta is not a JSON object: %w", err)
	}
	m := Meta{Schema: w.Schema, KeyID: w.KeyID, SuccessorOf: w.SuccessorOf, KeySHA256: strings.ToLower(strings.TrimSpace(w.KeySHA256))}
	if m.Schema == 0 {
		m.Schema = MetaSchema
	}
	if m.KeySHA256 != "" && !isHex64(m.KeySHA256) {
		return Meta{}, errors.New("credfile: meta key_sha256 is not 64 hex digits")
	}
	var err error
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		dst  *time.Time
	}{{"expires_at", w.ExpiresAt, &m.ExpiresAt}, {"issued_at", w.IssuedAt, &m.IssuedAt}, {"renew_after", w.RenewAfter, &m.RenewAfter}} {
		if *f.dst, err = parseMetaTime(f.raw); err != nil {
			return Meta{}, fmt.Errorf("credfile: meta %s: %w", f.name, err)
		}
	}
	return m, nil
}

func parseMetaTime(raw json.RawMessage) (time.Time, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return time.Time{}, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return time.Time{}, err
		}
		if s = strings.TrimSpace(s); s == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, errors.New("want an RFC 3339 time")
		}
		return t.UTC(), nil
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f < 0 || f > math.MaxInt32*4 || math.IsNaN(f) {
		return time.Time{}, errors.New("want an RFC 3339 string or unix seconds")
	}
	if f == 0 {
		return time.Time{}, nil
	}
	return time.Unix(int64(f), 0).UTC(), nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

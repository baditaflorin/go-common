// Package strictjson decodes one JSON value while rejecting duplicate object
// keys, unknown struct fields, and trailing values. Callers remain responsible
// for bounding the input bytes before decoding.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Decode decodes exactly one JSON value into dst with duplicate and unknown
// field rejection at every nesting level.
func Decode(data []byte, dst any) error {
	if len(data) == 0 || dst == nil {
		return errors.New("JSON input and destination are required")
	}
	if err := rejectDuplicates(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("JSON input must contain exactly one value")
	}
	return nil
}

func rejectDuplicates(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consume(decoder); err != nil {
		return errors.New("JSON input is invalid or contains duplicate object keys")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON input must contain exactly one value")
	}
	return nil
}

func consume(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := consume(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("JSON object is unterminated")
		}
	case '[':
		for decoder.More() {
			if err := consume(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("JSON array is unterminated")
		}
	default:
		return errors.New("JSON delimiter is invalid")
	}
	return nil
}

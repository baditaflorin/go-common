package deploymentintent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DecodeV1 parses one bounded JSON intent, rejecting duplicate keys and
// unknown fields before applying the schema's time-dependent validation.
func DecodeV1(body []byte, now time.Time) (V1, error) {
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return V1{}, fmt.Errorf("input must be between 1 byte and %d bytes", MaxBodyBytes)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return V1{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var intent V1
	if err := decoder.Decode(&intent); err != nil {
		return V1{}, fmt.Errorf("decode schema v1: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return V1{}, errors.New("input must contain exactly one JSON object")
		}
		return V1{}, fmt.Errorf("trailing input: %w", err)
	}
	if err := ValidateV1(intent, now); err != nil {
		return V1{}, err
	}
	return intent, nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := consumeJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid JSON structure: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("input must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
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
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

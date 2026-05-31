package response

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeData reads a fleet response envelope from r and unmarshals its
// "data" object into v.
//
// The fleet envelope (see Success / NewError) is:
//
//	{"status":"success","data":{...}}          // success
//	{"status":"error","error":{"code":...}}    // failure
//
// Consumers that hand-rolled a *top-level* decode (e.g. a bare
// struct{ Value string `json:"value"` }) silently missed the nested
// data object and read zero values instead. That exact mismatch broke
// the go-fleet-dns-sync reconciler for 10 days in 2026-05 — it decoded
// the vault's secret at the top level while the value lived under
// "data". DecodeData defines the contract once so it can't drift
// per-consumer.
//
// Behaviour:
//   - status == "error" (or an error object is present): returns the
//     decoded *Error (which implements error) so callers can switch on
//     Code / ErrorCode.
//   - status == "success" with data: json.Unmarshal(data, v). Pass
//     v == nil to validate the envelope without unmarshalling.
//   - success but data absent/null while v != nil: returns an error
//     (an empty data object is almost always a producer bug the caller
//     wants surfaced rather than a silent zero value).
//
// DecodeData never includes the raw body in its returned errors, so a
// decode failure on a secret-bearing response will not leak the secret.
func DecodeData(r io.Reader, v any) error {
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return fmt.Errorf("response: decode envelope: %w", err)
	}
	if env.Status == "error" || env.Error != nil {
		if env.Error != nil {
			return env.Error
		}
		return fmt.Errorf("response: status=error with no error detail")
	}
	if v == nil {
		return nil
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("response: success envelope carries no data")
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		// Deliberately do not echo the raw data — it may be a secret.
		return fmt.Errorf("response: unmarshal data into %T: %w", v, err)
	}
	return nil
}

// DecodeDataBytes is the []byte convenience wrapper around DecodeData.
func DecodeDataBytes(b []byte, v any) error {
	return DecodeData(bytes.NewReader(b), v)
}

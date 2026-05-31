package response

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeData_SuccessNested(t *testing.T) {
	// Exactly the vault read shape that broke go-fleet-dns-sync: the
	// value lives under data, not at the top level.
	body := `{"status":"success","data":{"name":"hcloud_token","value":"sekret"}}`
	var v struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := DecodeDataBytes([]byte(body), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Value != "sekret" || v.Name != "hcloud_token" {
		t.Fatalf("got %+v", v)
	}
}

func TestDecodeData_TopLevelValueIsIgnored(t *testing.T) {
	// A top-level "value" must NOT satisfy the decode — data is the
	// only source of truth. This is the regression the helper prevents.
	body := `{"status":"success","value":"wrong","data":{"value":"right"}}`
	var v struct {
		Value string `json:"value"`
	}
	if err := DecodeDataBytes([]byte(body), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Value != "right" {
		t.Fatalf("expected data.value=right, got %q", v.Value)
	}
}

func TestDecodeData_ErrorEnvelope(t *testing.T) {
	body := `{"status":"error","error":{"code":403,"error_code":"auth.scope_mismatch","message":"nope"}}`
	var v struct{ Value string }
	err := DecodeDataBytes([]byte(body), &v)
	if err == nil {
		t.Fatal("expected error")
	}
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("expected *response.Error, got %T: %v", err, err)
	}
	if re.Code != 403 || re.ErrorCode != "auth.scope_mismatch" {
		t.Fatalf("error not decoded: %+v", re)
	}
}

func TestDecodeData_MissingData(t *testing.T) {
	body := `{"status":"success"}`
	var v struct{ Value string }
	if err := DecodeDataBytes([]byte(body), &v); err == nil {
		t.Fatal("expected error for success envelope with no data")
	}
}

func TestDecodeData_NilValidatesOnly(t *testing.T) {
	if err := DecodeDataBytes([]byte(`{"status":"success","data":{"x":1}}`), nil); err != nil {
		t.Fatalf("nil v should validate without unmarshalling: %v", err)
	}
	if err := DecodeDataBytes([]byte(`{"status":"error","error":{"code":500,"message":"boom"}}`), nil); err == nil {
		t.Fatal("nil v should still surface an error envelope")
	}
}

func TestDecodeData_NeverLeaksBodyOnUnmarshalError(t *testing.T) {
	// data is a JSON string but v expects an object → unmarshal fails.
	// The returned error must not contain the secret payload.
	body := `{"status":"success","data":"super-secret-token-value"}`
	var v struct {
		Value string `json:"value"`
	}
	err := DecodeDataBytes([]byte(body), &v)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if strings.Contains(err.Error(), "super-secret-token-value") {
		t.Fatalf("error leaked body: %v", err)
	}
}

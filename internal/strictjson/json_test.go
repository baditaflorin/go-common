package strictjson

import (
	"strings"
	"testing"
)

func TestDecodeStrictlyRejectsDuplicatesUnknownFieldsAndTrailingValues(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	for _, input := range []string{
		`{"name":"first","name":"second"}`,
		`{"name":"ok","extra":true}`,
		`{"name":"ok"}{"name":"second"}`,
		`{"name":"ok","nested":{"x":1,"x":2}}`,
		`{"name":`,
	} {
		t.Run(strings.ReplaceAll(input, "/", "_"), func(t *testing.T) {
			var got payload
			if err := Decode([]byte(input), &got); err == nil {
				t.Fatalf("Decode accepted invalid input %q", input)
			}
		})
	}
}

func TestDecodeAcceptsOneStrictJSONValue(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	var got payload
	if err := Decode([]byte(` { "name" : "allowed" } `), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "allowed" {
		t.Fatalf("decoded name = %q", got.Name)
	}
}

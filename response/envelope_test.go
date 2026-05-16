package response

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixedClock(t *testing.T) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time {
		return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() { nowFunc = prev })
}

func clearIdentity(t *testing.T) {
	t.Helper()
	prev := serviceID
	serviceID = ""
	t.Cleanup(func() { serviceID = prev })
}

func TestEnvelope_NilData(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	got := Envelope(nil, 1)
	if got["_schema_version"] != 1 {
		t.Errorf("schema_version: got %v want 1", got["_schema_version"])
	}
	if _, ok := got["_emitted_at"]; !ok {
		t.Errorf("missing _emitted_at")
	}
	if _, ok := got["_service"]; ok {
		t.Errorf("_service should be omitted when identity unset")
	}
	if len(got) != 2 {
		t.Errorf("unexpected extra keys: %#v", got)
	}
}

func TestEnvelope_StructData(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	type payload struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	got := Envelope(payload{Status: "ok", Count: 7}, 3)
	if got["status"] != "ok" {
		t.Errorf("status missing or wrong: %v", got["status"])
	}
	// JSON unmarshal into any yields float64 for ints
	if v, ok := got["count"].(float64); !ok || v != 7 {
		t.Errorf("count: got %v (%T) want 7", got["count"], got["count"])
	}
	if got["_schema_version"] != 3 {
		t.Errorf("schema_version: got %v want 3", got["_schema_version"])
	}
}

func TestEnvelope_MapData(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	got := Envelope(map[string]any{"a": 1, "b": "two"}, 2)
	if got["a"] != 1 || got["b"] != "two" {
		t.Errorf("map keys not merged: %#v", got)
	}
	if got["_schema_version"] != 2 {
		t.Errorf("schema_version: %v", got["_schema_version"])
	}
}

func TestEnvelope_SchemaZeroOmitted(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	got := Envelope(map[string]any{"x": 1}, 0)
	if _, ok := got["_schema_version"]; ok {
		t.Errorf("schema_version=0 should be omitted, got %v", got["_schema_version"])
	}
	if got["x"] != 1 {
		t.Errorf("payload key dropped: %#v", got)
	}
}

func TestEnvelope_NestedPreserved(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	type inner struct {
		Deep string `json:"deep"`
	}
	type outer struct {
		Inner inner    `json:"inner"`
		Tags  []string `json:"tags"`
	}
	got := Envelope(outer{Inner: inner{Deep: "value"}, Tags: []string{"a", "b"}}, 1)
	in, ok := got["inner"].(map[string]any)
	if !ok {
		t.Fatalf("inner missing or wrong type: %#v", got["inner"])
	}
	if in["deep"] != "value" {
		t.Errorf("nested value not preserved: %#v", in)
	}
	tags, ok := got["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("nested slice not preserved: %#v", got["tags"])
	}
}

func TestEnvelope_NonObjectJSONLandsUnderData(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	got := Envelope([]int{1, 2, 3}, 1)
	d, ok := got["data"].([]any)
	if !ok || len(d) != 3 {
		t.Errorf("array not parked under data: %#v", got["data"])
	}
}

func TestEnvelope_ServiceIDInjected(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)
	SetServiceID("go_demo")
	got := Envelope(nil, 1)
	if got["_service"] != "go_demo" {
		t.Errorf("_service not injected: %#v", got)
	}
}

func TestEnvelope_ConflictPrefersUserAndWarns(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)
	SetServiceID("go_demo")

	// Redirect stderr to capture the warning.
	r, w, _ := os.Pipe()
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	got := Envelope(map[string]any{"_service": "user_wins"}, 1)

	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	if got["_service"] != "user_wins" {
		t.Errorf("expected user payload to win on conflict, got %v", got["_service"])
	}
	if !strings.Contains(buf.String(), "_service") || !strings.Contains(buf.String(), "conflicts") {
		t.Errorf("expected stderr warning, got: %q", buf.String())
	}
}

func TestEnvelope_JSONRoundTrips(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)

	type payload struct {
		Hello string `json:"hello"`
	}
	got := Envelope(payload{Hello: "world"}, 4)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["hello"] != "world" {
		t.Errorf("roundtrip lost field: %#v", back)
	}
	if back["_schema_version"].(float64) != 4 {
		t.Errorf("roundtrip schema_version: %v", back["_schema_version"])
	}
}

func TestEnvelope_NoMutateInput(t *testing.T) {
	fixedClock(t)
	clearIdentity(t)
	in := map[string]any{"k": "v"}
	snapshot := map[string]any{"k": "v"}
	_ = Envelope(in, 1)
	if !reflect.DeepEqual(in, snapshot) {
		t.Errorf("input map was mutated: got %#v want %#v", in, snapshot)
	}
}

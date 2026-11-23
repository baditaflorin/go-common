package durablequeue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestAttemptClientFencesWritesAndValidatesReads(t *testing.T) {
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			var v map[string]any
			if json.NewDecoder(r.Body).Decode(&v) != nil || v["lease_version"] != float64(3) || v["worker"] != "w" || !strings.HasSuffix(r.URL.Path, "/attempt") {
				t.Fatal("missing lease fence")
			}
			return response(200, `{"recorded":true}`), nil
		}
		return response(200, `{"attempts":[{"lease_version":3,"attempt":1,"payload":{"error":"proxy"},"created_at":"2030-01-01T00:00:00Z"}]}`), nil
	})})
	l := Lease{Key: "site", Worker: "w", Version: 3}
	if err := c.RecordAttempt(context.Background(), "run", l, json.RawMessage(`{"error":"proxy"}`)); err != nil {
		t.Fatal(err)
	}
	a, err := c.AttemptReceipts(context.Background(), "run", "site")
	if err != nil || len(a) != 1 || a[0].Attempt != 1 {
		t.Fatal(a, err)
	}
	if err = c.RecordAttempt(context.Background(), "run", l, json.RawMessage(`null`)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	bad, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) { return response(200, `{"attempts":null}`), nil })})
	if _, err = bad.AttemptReceipts(context.Background(), "run", "site"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

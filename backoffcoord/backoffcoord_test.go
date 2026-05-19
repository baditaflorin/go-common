package backoffcoord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConsultUnreachableFailsOpen(t *testing.T) {
	c := &Client{
		BaseURL: "http://127.0.0.1:1", // guaranteed-unreachable
		HTTP:    &http.Client{Timeout: 100 * time.Millisecond},
	}
	res, err := c.Consult(context.Background(), "example.com", Failure{Status: 500})
	if err != nil {
		t.Fatalf("Consult should never error on coordinator outage: %v", err)
	}
	if !res.FellOpen {
		t.Fatal("FellOpen should be true on unreachable coordinator")
	}
	if res.Wait != 0 {
		t.Fatalf("Wait = %v, want 0 on fail-open", res.Wait)
	}
}

func TestConsultReturnsWaitMS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"wait_ms": 250})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), MaxWait: time.Second}
	res, err := c.Consult(context.Background(), "h", Failure{Status: 500})
	if err != nil {
		t.Fatal(err)
	}
	if res.Wait != 250*time.Millisecond {
		t.Fatalf("Wait = %v, want 250ms", res.Wait)
	}
}

func TestConsultCapsAtMaxWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"wait_ms": 60_000})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), MaxWait: 500 * time.Millisecond}
	res, _ := c.Consult(context.Background(), "h", Failure{Status: 500})
	if res.Wait != 500*time.Millisecond {
		t.Fatalf("Wait = %v, want capped at 500ms", res.Wait)
	}
}

func TestObserverFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"wait_ms": 0})
	}))
	defer srv.Close()
	var got Event
	c := (&Client{BaseURL: srv.URL, HTTP: srv.Client()}).SetObserver(observerFunc(func(ev Event) { got = ev }))
	_, _ = c.Consult(context.Background(), "h", Failure{Status: 500})
	if got.Host != "h" || got.Outcome != "no_wait" {
		t.Fatalf("event = %+v, want host=h outcome=no_wait", got)
	}
}

type observerFunc func(Event)

func (f observerFunc) ObserveBackoffCoord(ev Event) { f(ev) }

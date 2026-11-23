package durablequeue

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestTerminalResultsPageAndCursor(t *testing.T) {
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/v1/runs/run/tasks" || r.URL.Query().Get("after") != "site/01" || r.URL.Query().Get("limit") != "2" {
			t.Fatalf("bad request: %s %s", r.Method, r.URL.String())
		}
		return response(200, `{"tasks":[{"key":"site-02","state":"done","attempts":1,"result":{"id":"capture"}},{"key":"site-03","state":"failed","attempts":3,"error_code":"timeout"}],"next":"site-03"}`), nil
	})})
	p, err := c.TerminalResults(context.Background(), "run", "site/01", 2)
	if err != nil || len(p.Tasks) != 2 || p.Next != "site-03" || p.Tasks[1].ErrorCode != "timeout" {
		t.Fatalf("page=%+v err=%v", p, err)
	}
}
func TestTerminalResultsRejectsMalformedServerPages(t *testing.T) {
	for _, body := range []string{
		`{"tasks":[{"key":"a","state":"running","attempts":1}]}`,
		`{"tasks":[{"key":"a","state":"done","attempts":1}]}`,
		`{"tasks":[{"key":"a","state":"failed","attempts":1}]}`,
		`{"tasks":[{"key":"a","state":"done","attempts":1,"result":{}}],"next":"different"}`,
		`{"tasks":[{"key":"a","state":"done","attempts":1,"result":{}},{"key":"a","state":"done","attempts":1,"result":{}}]}`,
	} {
		c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) { return response(200, body), nil })})
		if _, err := c.TerminalResults(context.Background(), "run", "", 10); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %s: %v", body, err)
		}
	}
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat("a", (1<<20)+1)), nil })})
	if _, err := c.TerminalResults(context.Background(), "run", "", 10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestRunCompletionRejectsIncompleteOrContradictoryResponses(t *testing.T) {
	for _, tc := range []struct {
		body     string
		valid    bool
		complete bool
	}{
		{`{"sealed":true,"complete":true}`, true, true},
		{`{"sealed":false,"complete":false}`, true, false},
		{`{"sealed":false,"complete":true}`, false, false},
		{`{"complete":false}`, false, false},
		{`{"sealed":true}`, false, false},
	} {
		c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/runs/run/completion" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			return response(200, tc.body), nil
		})})
		result, err := c.RunCompletion(context.Background(), "run")
		if tc.valid {
			if err != nil || result.Complete != tc.complete {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		} else if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted %s: %v", tc.body, err)
		}
	}
}

package httpmock_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/testhelpers/httpmock"
)

func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get("Content-Type")
}

func TestJSONServesTheBodyAndCountsCalls(t *testing.T) {
	srv := httpmock.JSON(t, http.StatusCreated, map[string]int{"n": 1})
	status, body, ctype := get(t, srv.URL+"/items")
	if status != http.StatusCreated || body != `{"n":1}` || ctype != "application/json" {
		t.Fatalf("got %d %q %q", status, body, ctype)
	}
	get(t, srv.URL+"/items")
	get(t, srv.URL+"/other")
	srv.AssertCalls(t, "/items", 2)
	srv.AssertCalls(t, "/other", 1)
	srv.AssertAtLeast(t, "/items", 1)
	if got := srv.CallCount("/never"); got != 0 {
		t.Fatalf("CallCount of an untouched path = %d, want 0", got)
	}
}

func TestStatusServesOnlyAStatus(t *testing.T) {
	srv := httpmock.Status(t, http.StatusTeapot)
	if status, body, _ := get(t, srv.URL); status != http.StatusTeapot || body != "" {
		t.Fatalf("got %d %q", status, body)
	}
}

// Sequence plays its responses in order and keeps answering with the last one.
func TestSequenceRepeatsTheLastResponse(t *testing.T) {
	srv := httpmock.Sequence(t,
		httpmock.RespondStatus(http.StatusServiceUnavailable),
		httpmock.RespondJSON(http.StatusOK, map[string]string{"ok": "yes"}),
	)
	var statuses []int
	for i := 0; i < 4; i++ {
		s, _, _ := get(t, srv.URL)
		statuses = append(statuses, s)
	}
	want := []int{503, 200, 200, 200}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", statuses, want)
		}
	}
}

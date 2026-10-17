package meshclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/baditaflorin/go-common/degraded"
	"github.com/baditaflorin/go-common/meshclient"
)

// A fail-open client: when the sibling is down the caller carries on without its
// answer, and the request's degraded list says what is missing.
func ExampleClient_Do_failOpen() {
	sibling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer sibling.Close()

	enrich, _ := meshclient.New(meshclient.Config{Name: "geo", BaseURL: sibling.URL, Mode: meshclient.FailOpen, Timeout: time.Second})

	sink := degraded.New() // one per request; its Slice() goes in the response envelope
	ctx := degraded.NewContext(context.Background(), sink)

	var out struct {
		Country string `json:"country"`
	}
	err := enrich.Get(ctx, "/v1/lookup", &out)
	if meshclient.IsUnavailable(err) {
		fmt.Println("carrying on without geo")
	}
	fmt.Println("degraded:", sink.Slice())
	// Output:
	// carrying on without geo
	// degraded: [geo-down]
}

// A fail-closed client, and the difference between the sibling being unavailable
// and the sibling saying no: a 402 is an answer, with its body.
func ExampleStatusError() {
	sibling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"required":5,"balance":2}`)
	}))
	defer sibling.Close()

	ledger, _ := meshclient.New(meshclient.Config{Name: "ledger", BaseURL: sibling.URL, Mode: meshclient.FailClosed, Timeout: time.Second})
	err := ledger.PostJSON(context.Background(), "/v1/charge", map[string]int{"amount": 5}, nil)

	var status *meshclient.StatusError
	if errors.As(err, &status) {
		fmt.Println(status.Status, string(status.Body))
	}
	fmt.Println("unavailable:", meshclient.IsUnavailable(err))
	// Output:
	// 402 {"required":5,"balance":2}
	// unavailable: false
}

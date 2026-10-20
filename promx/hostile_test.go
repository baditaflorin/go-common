package promx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestHTTPServesARequestWhoseTargetIsNotUTF8: GET /%ff reaches a handler as the
// path "/\xff". The middleware used to pass it to client_golang, which panics on
// a label value that is not valid UTF-8, after the handler had run: net/http
// recovered, logged a stack trace and closed the connection without the reply. One
// request, from anyone who could reach the port, before authentication. Over a real
// server, so the decoding is Go's own.
func TestHTTPServesARequestWhoseTargetIsNotUTF8(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg)
	var served int
	srv := httptest.NewServer(coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		_, _ = io.WriteString(w, "ok")
	})))
	defer srv.Close()

	for _, target := range []string{"/%ff", "/a/%c0%af", "/" + strings.Repeat("a", 5000), "/%00%0d%0a%22"} {
		resp, err := http.Get(srv.URL + target)
		if err != nil {
			t.Fatalf("GET %.20s: %v (the connection was cut: the middleware panicked?)", target, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %.20s = %d, want 200", target, resp.StatusCode)
		}
	}
	if served != 4 {
		t.Errorf("handler ran %d times, want 4", served)
	}
	// Three of the four are unusable as a label and count under _other; the control
	// characters are valid UTF-8 and short, so that route is named.
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", OtherLabel, "200")); got != 3 {
		t.Errorf(`requests_total{route="_other"} = %v, want 3`, got)
	}
}

// TestEgressObservesAHostThatIsNotUTF8: http://%ff.example/ parses, and its host is
// "\xff.example". Once such a host had been seen minHits times it was admitted and
// the next event panicked inside the caller's RoundTrip or response-body Close,
// which in a worker goroutine ends the process.
func TestEgressObservesAHostThatIsNotUTF8(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewEgressCollectors(reg)
	for range 3 * defaultHostMinHits {
		coll.ObserveEgress(safehttp.EgressEvent{Host: "\xff.example", Scheme: "https", Outcome: safehttp.OutcomeDNSFail, Duration: time.Millisecond})
	}
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, OtherLabel, "https", "false", "dns_fail")); got != 3*defaultHostMinHits {
		t.Errorf(`requests_total{host="_other"} = %v, want %d`, got, 3*defaultHostMinHits)
	}
}

func TestSchemeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"http": "http", "https": "https", "": UnknownLabel,
		"HTTP": OtherLabel, "ftp": OtherLabel, "a1+b.c-d": OtherLabel, "\xff": OtherLabel,
	} {
		if got := schemeLabel(in); got != want {
			t.Errorf("schemeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusLabel(t *testing.T) {
	for in, want := range map[int]string{
		200: "200", 204: "204", 301: "301", 404: "404", 429: "429", 503: "503", 100: "100", 101: "101",
		299: "2xx", 306: "3xx", 499: "4xx", 520: "5xx", 599: "5xx", 199: "1xx",
		600: OtherLabel, 999: OtherLabel, 99: OtherLabel, 0: OtherLabel, -1: OtherLabel, 1000: OtherLabel,
	} {
		if got := statusLabel(in); got != want {
			t.Errorf("statusLabel(%d) = %q, want %q", in, got, want)
		}
	}
	// A registered code is never folded, so every dashboard that selects one still can.
	for code := 100; code <= 599; code++ {
		if http.StatusText(code) != "" && statusLabel(code) != strconv.Itoa(code) {
			t.Errorf("registered code %d is labelled %q", code, statusLabel(code))
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = statusLabel(503) }); allocs != 0 {
		t.Errorf("statusLabel allocates %v times", allocs)
	}
}

package selftest

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// recordingDelegate records every URL it is asked to fetch.
type recordingDelegate struct{ seen []string }

func (d *recordingDelegate) FetchGet(_ context.Context, url string, _ http.Header) (*safehttp.FetchResult, error) {
	d.seen = append(d.seen, url)
	return &safehttp.FetchResult{Status: 200, Body: []byte("from-cache")}, nil
}

// A check that does an outbound safehttp GET while a default fetch-cache
// delegate is installed must NOT route through the delegate — the suite
// injects WithoutFetchCacheContext so /selftest validates the real origin.
func TestSuite_ChecksBypassFetchCache(t *testing.T) {
	safehttp.SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { safehttp.SetAllowedPrivateIPs(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "from-origin")
	}))
	t.Cleanup(srv.Close)

	d := &recordingDelegate{}
	safehttp.SetDefaultFetchDelegate(d)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	s := NewSuite("test-svc", "0.0.0")
	var gotBody string
	s.Check("outbound", func(ctx context.Context) error {
		c := safehttp.NewClient()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		gotBody = string(b)
		return nil
	})

	resp := s.run(context.Background(), CategoryAny)
	if !resp.OK {
		t.Fatalf("suite failed: %+v", resp.Checks)
	}
	if gotBody != "from-origin" {
		t.Fatalf("check fetched %q, want direct origin response (cache was bypassed)", gotBody)
	}
	if len(d.seen) != 0 {
		t.Fatalf("selftest check must bypass the fetch-cache delegate, but it saw %v", d.seen)
	}
}

package graph

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAlternateRouteKeepsBatchIdentityAndNeverRetriesAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		primaryStatus int
		primaryErr    bool
		altStatus     int
		wantAlt       bool
		wantPending   bool
	}{
		{"unreachable", 0, true, 200, true, false},
		{"unavailable", 503, false, 200, true, false},
		{"redirect", 302, false, 200, true, false},
		{"auth", 401, false, 200, false, true},
		{"forbidden", 403, false, 200, false, false},
		{"rate-limited", 429, false, 200, false, true},
		{"alternate-reject", 503, false, 403, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{enabled: true, collectorURL: "https://" + graphCanonicalCollectorHost, writerAPIKey: "key", alternateIP: netip.MustParseAddr("127.0.0.1")}
			s, r, _ := newTestSender(cfg, 1)
			var firstID, firstBody string
			calls := 0
			s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				firstID = req.Header.Get(BatchIDHeader)
				b, _ := io.ReadAll(req.Body)
				firstBody = string(b)
				if tc.primaryErr {
					return nil, errors.New("injected failure")
				}
				return &http.Response{StatusCode: tc.primaryStatus, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: req}, nil
			})
			s.alternateClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				b, _ := io.ReadAll(req.Body)
				if firstID == "" || req.Header.Get(BatchIDHeader) != firstID || string(b) != firstBody {
					t.Error("retry did not preserve identity and body")
				}
				h := make(http.Header)
				h.Set("X-Service-Name", "go-fleet-graph")
				return &http.Response{StatusCode: tc.altStatus, Body: io.NopCloser(strings.NewReader("")), Header: h, Request: req}, nil
			})
			_, _ = r.push(Event{Path: "/test"})
			s.flush()
			if (calls > 0) != tc.wantAlt {
				t.Fatalf("alternate calls=%d", calls)
			}
			if (len(s.pending) > 0) != tc.wantPending {
				t.Fatalf("pending=%d", len(s.pending))
			}
		})
	}
}

func TestAlternateIPValidationAndCanonicalOnly(t *testing.T) {
	for _, raw := range []string{"not-a-host", "1.2.3.4:443", "0.0.0.0", "ff02::1", "fe80::1%lo0"} {
		if _, err := parseAlternateCollectorIP(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "2001:db8::1"} {
		if _, err := parseAlternateCollectorIP(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	cfg := config{enabled: true, collectorURL: "http://127.0.0.1:1234", writerAPIKey: "key", alternateIP: netip.MustParseAddr("127.0.0.1")}
	s, _, _ := newTestSender(cfg, 1)
	if s.alternateClient != nil {
		t.Fatal("alternate enabled for noncanonical URL")
	}
	c := newGraphAlternateHTTPClientForAddress("127.0.0.1:1")
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://not-graph.example/", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("noncanonical authority allowed")
	}
}

func TestAlternateTLSChecksCanonicalHostnameAndDoesNotFollowRedirect(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: graphCanonicalCollectorHost}, DNSNames: []string{graphCanonicalCollectorHost}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	count := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.TLS.ServerName != graphCanonicalCollectorHost {
			t.Error("wrong SNI")
		}
		http.Redirect(w, r, "https://other.example/", http.StatusFound)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	c := newGraphAlternateHTTPClientForAddress(srv.Listener.Addr().String())
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	res, err := c.Get("https://" + graphCanonicalCollectorHost + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 302 || count != 1 {
		t.Fatalf("status=%d requests=%d", res.StatusCode, count)
	}
	c2 := newGraphAlternateHTTPClientForAddress(srv.Listener.Addr().String())
	if _, err := c2.Get("https://" + graphCanonicalCollectorHost + "/"); err == nil {
		t.Fatal("untrusted alternate certificate accepted")
	}
}

func TestAlternateUnrelatedSuccessDoesNotDropBatch(t *testing.T) {
	cfg := config{enabled: true, collectorURL: "https://" + graphCanonicalCollectorHost, writerAPIKey: "key", alternateIP: netip.MustParseAddr("127.0.0.1")}
	s, r, _ := newTestSender(cfg, 1)
	s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, errors.New("primary unavailable") })
	s.alternateClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("unrelated")), Header: make(http.Header), Request: req}, nil
	})
	_, _ = r.push(Event{Path: "/test"})
	s.flush()
	if len(s.pending) != 1 {
		t.Fatal("unidentified alternate success discarded the batch")
	}
}

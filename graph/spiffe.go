package graph

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The SPIFFE path for event batches.
//
// When GRAPH_SPIFFE_COLLECTOR_URL, GRAPH_SPIFFE_COLLECTOR_ID and
// SPIFFE_ENDPOINT_SOCKET are all set, the sender POSTs each batch to the
// collector's private mTLS listener and proves its identity with the
// workload's X.509-SVID instead of the writer key. The writer key, when one is
// configured, stays the fallback: a batch whose mTLS send fails on the path
// (no connection, a refused handshake, 401, 404, 405, 421) is sent once more
// with the key in the same flush, and the next minute of batches use the key.
// An answer from the collector itself (2xx, a content refusal, 408, 429, 5xx)
// is final for that batch, so a slow collector is not sent every batch twice.
//
// graph does not speak the Workload API itself: that would put go-spiffe,
// go-jose and gRPC under every package that imports graph, safehttp included
// (scripts/dep-budget.txt). It uses the client an MTLSClientFactory returns;
// server.New installs one built on package spiffe, and a service that does not
// use server.New calls SetMTLSClientFactory itself.

const (
	// mtlsPathFailureCooldown is how long batches use the writer key after the
	// mTLS path failed.
	mtlsPathFailureCooldown = time.Minute
	// mtlsConnectTimeout bounds one wait for the first SVID.
	mtlsConnectTimeout  = 30 * time.Second
	mtlsConnectRetryMin = 5 * time.Second
	mtlsConnectRetryMax = 5 * time.Minute
)

// MTLSClient is what the sender needs from a SPIFFE mTLS HTTP client.
// *spiffe.HTTPClient satisfies it.
type MTLSClient interface {
	Do(*http.Request) (*http.Response, error)
	Close() error
}

// MTLSClientFactory connects to the Workload API at socket and returns a
// client that presents this workload's X.509-SVID and accepts only a server
// that proves one of allowedServerIDs, with timeout as its total request
// timeout. It may block until the first SVID arrives or ctx ends.
type MTLSClientFactory func(ctx context.Context, socket string, allowedServerIDs []string, timeout time.Duration) (MTLSClient, error)

var (
	mtlsFactoryMu    sync.Mutex
	mtlsFactory      MTLSClientFactory
	mtlsFactoryReady = make(chan struct{})
)

// SetMTLSClientFactory installs the factory the sender uses when the SPIFFE
// path is configured. The first non-nil factory wins and later calls are
// ignored, so a service that installs its own before server.New keeps it. A
// sender that starts before any factory is installed waits for one and uses
// the writer key meanwhile.
func SetMTLSClientFactory(f MTLSClientFactory) {
	if f == nil {
		return
	}
	mtlsFactoryMu.Lock()
	defer mtlsFactoryMu.Unlock()
	if mtlsFactory != nil {
		return
	}
	mtlsFactory = f
	close(mtlsFactoryReady)
}

func currentMTLSFactory() (MTLSClientFactory, <-chan struct{}) {
	mtlsFactoryMu.Lock()
	defer mtlsFactoryMu.Unlock()
	return mtlsFactory, mtlsFactoryReady
}

// normalizeSPIFFECollectorURL accepts the root of an HTTPS endpoint. Unlike
// GRAPH_COLLECTOR_URL any host is allowed: the server is authenticated by its
// SPIFFE ID, not by its name, and no writer key is sent on this path.
func normalizeSPIFFECollectorURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("graph SPIFFE collector URL: %w", err)
	}
	switch {
	case !strings.EqualFold(u.Scheme, "https"):
		return "", errors.New("graph SPIFFE collector URL: https is required")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("graph SPIFFE collector URL: a host is required")
	case u.User != nil:
		return "", errors.New("graph SPIFFE collector URL: user info is not allowed")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("graph SPIFFE collector URL: query and fragment are not allowed")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("graph SPIFFE collector URL: endpoint must not contain a path")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// validSPIFFEIDShape is a shape check only. The factory parses the ID
// strictly and refuses a malformed one, which leaves the path unavailable.
func validSPIFFEIDShape(id string) bool {
	return strings.HasPrefix(id, "spiffe://") && len(id) > len("spiffe://") && len(id) <= 2048 &&
		!strings.ContainsAny(id, " \t\r\n?#")
}

// connectMTLS waits for a factory and then for the first SVID, retrying with
// a growing pause until it has a client or the sender stops.
func (s *sender) connectMTLS() {
	defer close(s.mtlsConnected)
	_, ready := currentMTLSFactory()
	select {
	case <-ready:
	case <-s.stop:
		return
	}
	pause := s.mtlsRetryPause
	for attempt := 1; ; attempt++ {
		factory, _ := currentMTLSFactory()
		ctx, cancel := context.WithTimeout(context.Background(), mtlsConnectTimeout)
		go func() {
			select {
			case <-s.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		client, err := factory(ctx, s.cfg.spiffeSocket, []string{s.cfg.spiffeCollectorID}, sendTimeout)
		cancel()
		if err == nil && client != nil {
			if !s.setMTLSClient(client) {
				_ = client.Close() // the sender stopped while it connected
				return
			}
			fallback := "the writer key is the fallback"
			if !s.cfg.keyPathConfigured() {
				fallback = "no writer key is configured, so batches wait while this path is unavailable"
			}
			log.Printf("graph: SPIFFE mTLS path ready: batches go to %s, collector %s; %s", s.cfg.spiffeCollectorURL, s.cfg.spiffeCollectorID, fallback)
			return
		}
		select {
		case <-s.stop:
			return
		default:
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("graph: SPIFFE mTLS path not available yet (attempt %d, retry in %s): %v", attempt, pause, err)
		}
		select {
		case <-s.stop:
			return
		case <-time.After(pause):
		}
		if pause *= 2; pause > mtlsConnectRetryMax {
			pause = mtlsConnectRetryMax
		}
	}
}

// setMTLSClient stores the connected client unless the sender has stopped.
func (s *sender) setMTLSClient(client MTLSClient) bool {
	s.mtlsMu.Lock()
	defer s.mtlsMu.Unlock()
	if s.mtlsClosed {
		return false
	}
	s.mtlsClient = client
	return true
}

// mtlsClientAt returns the client when the path is connected and not cooling
// down after a failure.
func (s *sender) mtlsClientAt(now time.Time) MTLSClient {
	s.mtlsMu.Lock()
	defer s.mtlsMu.Unlock()
	if s.mtlsClient == nil || now.Before(s.mtlsCooldownUntil) {
		return nil
	}
	return s.mtlsClient
}

// mtlsPathFailed starts the cool-down and logs once per cool-down.
func (s *sender) mtlsPathFailed(now time.Time, reason string) {
	s.mtlsMu.Lock()
	s.mtlsCooldownUntil = now.Add(mtlsPathFailureCooldown)
	s.mtlsMu.Unlock()
	next := "batches wait"
	if s.cfg.keyPathConfigured() {
		next = "batches use the writer key"
	}
	log.Printf("graph: SPIFFE mTLS send failed (%s); %s for %s", reason, next, mtlsPathFailureCooldown)
}

// closeMTLS closes the client once the final flush is done.
func (s *sender) closeMTLS() {
	s.mtlsMu.Lock()
	client := s.mtlsClient
	s.mtlsClient = nil
	s.mtlsClosed = true
	s.mtlsMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// mtlsPathFailure reports whether an answer on the mTLS path means the path
// is wrong (the listener does not take this workload, or is not the collector)
// rather than an answer about the batch.
func mtlsPathFailure(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusMisdirectedRequest:
		return true
	}
	return false
}

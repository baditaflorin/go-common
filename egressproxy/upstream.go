package egressproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/proxysupplier"
)

// maxAttempts is the most upstreams one request tries: the first, plus one
// retry on a different upstream when nothing was relayed yet.
const maxAttempts = 2

// firstAttemptShare is the part of DialTimeout the first attempt may use, so a
// hanging upstream still leaves time for the retry.
const firstAttemptShare = 0.6

// upstream is one parsed upstream proxy. It never exposes the credentialed
// URL: addr is host:port, auth is the ready Proxy-Authorization value.
type upstream struct {
	addr   string // host:port, no credentials
	scheme string
	auth   string // "Basic ..." or ""
}

var (
	errNoUpstream  = errors.New("no upstream proxy available")
	errBadUpstream = errors.New("upstream proxy URL unusable")
)

// pickUpstream asks the supplier for an upstream. attempt is 1 or 2; avoid is
// the addr that just failed (the retry prefers a different one). A returned
// error is one of errNoUpstream (nothing available, or a loop back to this
// proxy) or errBadUpstream (the supplier returned something unusable).
func (p *Proxy) pickUpstream(t Target, attempt int, avoid string) (*upstream, error) {
	if p.cfg.Supplier == nil {
		return nil, errNoUpstream
	}
	var up *upstream
	// The retry asks again a few times for a different upstream; a supplier
	// with a single upstream returns the same one and that is accepted.
	tries := 1
	if attempt > 1 {
		tries = 4
	}
	for i := 0; i < tries; i++ {
		raw := p.rawUpstreamURL(t, attempt)
		if raw == "" {
			return nil, errNoUpstream
		}
		u, err := parseUpstream(raw)
		if err != nil {
			return nil, err
		}
		if p.isSelf(u.addr) {
			return nil, errNoUpstream
		}
		up = u
		if avoid == "" || u.addr != avoid {
			break
		}
	}
	return up, nil
}

func (p *Proxy) rawUpstreamURL(t Target, attempt int) string {
	s := p.cfg.Supplier
	if attempt == 1 && p.cfg.KeyFunc != nil {
		if ks, ok := s.(proxysupplier.KeyedSupplier); ok {
			if key := p.cfg.KeyFunc(t); key != "" {
				if raw := ks.ProxyURLForKey(key); raw != "" {
					return raw
				}
			}
		}
	}
	return s.ProxyURL()
}

// parseUpstream turns "http://user:pass@host:port" into an upstream. Errors
// are deliberately generic: url.Parse's own error text quotes the whole URL,
// credentials included.
func parseUpstream(raw string) (*upstream, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errBadUpstream
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" {
		return nil, errBadUpstream
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return nil, errBadUpstream
	}
	if port == "" {
		port = "80"
	}
	up := &upstream{addr: net.JoinHostPort(host, port), scheme: scheme}
	if u.User != nil {
		pass, _ := u.User.Password()
		up.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass))
	}
	return up, nil
}

// isSelf reports whether addr is this proxy's own listener, which would make
// the request loop forever.
func (p *Proxy) isSelf(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	_, selfPort, _ := net.SplitHostPort(p.addr)
	if port != selfPort {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// markResult reports an outcome to the supplier when it can take one.
func (p *Proxy) markResult(addr string, ok bool) {
	if rr, is := p.cfg.Supplier.(proxysupplier.ResultReporter); is {
		rr.MarkResult(addr, ok)
	}
}

// dialUpstream opens the TCP connection to the upstream proxy. This is the only
// direct dial in the package.
func (p *Proxy) dialUpstream(ctx context.Context, up *upstream) (net.Conn, error) {
	d := net.Dialer{KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", up.addr)
}

// attemptContext returns the context for one attempt: all attempts share the
// request's DialTimeout deadline and the first one may only use a share of it.
func (p *Proxy) attemptContext(parent context.Context, attempt int, start, deadline time.Time) (context.Context, context.CancelFunc) {
	d := deadline
	if attempt == 1 {
		d = start.Add(time.Duration(float64(deadline.Sub(start)) * firstAttemptShare))
	}
	return context.WithDeadline(parent, d)
}

// connectOutcome is what the CONNECT handshake with an upstream produced.
type connectOutcome struct {
	conn   net.Conn
	br     *bufio.Reader
	status int   // upstream status (0 when no answer)
	err    error // dial or handshake failure
	// dialFailed distinguishes "could not connect" from "connected but the
	// handshake failed" for the attempts metric.
	dialFailed bool
}

// connectVia dials up and asks it for a tunnel to authority ("host:port"),
// injecting Proxy-Authorization. The returned conn is registered with the
// session so Close and the idle watchdog reach it.
func (s *session) connectVia(ctx context.Context, up *upstream, authority string) connectOutcome {
	p := s.p
	raw, err := p.dialUpstream(ctx, up)
	if err != nil {
		return connectOutcome{err: err, dialFailed: true}
	}
	if !s.attach(raw) {
		return connectOutcome{err: net.ErrClosed}
	}
	conn := &activityConn{Conn: raw, wd: s.wd}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	// Abort a handshake that outlives the request (Close, DialTimeout).
	stopWatch := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	defer stopWatch()

	var b strings.Builder
	b.WriteString("CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n")
	if up.auth != "" {
		b.WriteString("Proxy-Authorization: " + up.auth + "\r\n")
	}
	b.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		s.detach()
		return connectOutcome{err: err}
	}
	br := bufio.NewReaderSize(conn, 4096)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		s.detach()
		return connectOutcome{err: err}
	}
	// Only the status matters. Do not touch resp.Body: for a 2xx answer to
	// CONNECT it is a read-until-EOF view of the tunnel itself, and closing it
	// would drain the stream. A non-2xx answer's connection is closed by the
	// caller.
	if !stopWatch() {
		// The request's context ended while we read the answer: the deadline
		// watcher has fired or is firing, so this connection is not safe to use.
		s.detach()
		return connectOutcome{err: context.DeadlineExceeded}
	}
	_ = raw.SetDeadline(time.Time{})
	return connectOutcome{conn: conn, br: br, status: resp.StatusCode}
}

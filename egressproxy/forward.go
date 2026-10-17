package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// hopHeaders are the headers that describe one connection, not the message.
// Proxy-* headers are removed separately by prefix.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// removeHopByHop drops the hop-by-hop headers, the headers the Connection
// header names, and every Proxy-* header.
func removeHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for k := range h {
		if len(k) >= 6 && strings.EqualFold(k[:6], "proxy-") {
			delete(h, k)
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// isUpgrade reports an Upgrade request (WebSocket) and the protocol asked for.
func isUpgrade(h http.Header) (string, bool) {
	proto := strings.TrimSpace(h.Get("Upgrade"))
	if proto == "" {
		return "", false
	}
	for _, v := range h["Connection"] {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return proto, true
			}
		}
	}
	return "", false
}

// forwardResult is a response head received from an upstream.
type forwardResult struct {
	conn net.Conn
	br   *bufio.Reader
	resp *http.Response
	// stage names where a failure happened: "dial" (nothing was sent) or
	// "exchange" (the request may have been sent).
	stage string
	err   error
}

// forwardVia sends the prepared request to one upstream and reads the response
// head, skipping interim 1xx answers other than 101.
func (s *session) forwardVia(ctx context.Context, req *http.Request, up *upstream) forwardResult {
	raw, err := s.p.dialUpstream(ctx, up)
	if err != nil {
		return forwardResult{stage: "dial", err: err}
	}
	if !s.attach(raw) {
		return forwardResult{stage: "dial", err: net.ErrClosed}
	}
	conn := &activityConn{Conn: raw, wd: s.wd}
	if up.auth != "" {
		req.Header.Set("Proxy-Authorization", up.auth)
	} else {
		req.Header.Del("Proxy-Authorization")
	}
	if err := req.WriteProxy(conn); err != nil {
		s.detach()
		return forwardResult{stage: "exchange", err: err}
	}
	br := bufio.NewReaderSize(conn, 4096)
	for {
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			s.detach()
			return forwardResult{stage: "exchange", err: err}
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			continue
		}
		return forwardResult{conn: conn, br: br, resp: resp}
	}
}

// writerOnly exposes only Write of the wrapped writer.
type writerOnly struct{ io.Writer }

// handleForward serves one absolute-URI plain HTTP request. It reports whether
// the client connection can carry another request.
func (s *session) handleForward(req *http.Request) bool {
	p := s.p
	start := time.Now()
	u := req.URL
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "80"
	}
	if u.Scheme != "http" || checkTarget(host, port) != nil {
		p.m.result(kindHTTP, resBadRequest)
		s.reply(http.StatusBadRequest, "egress proxy: only http:// absolute URIs are forwarded (https goes through CONNECT)")
		return false
	}
	t := Target{Kind: kindHTTP, Host: host, Port: port, Client: s.raw.RemoteAddr()}
	if !s.checkGuard(t) {
		return false
	}

	// Remember what the browser asked for before the headers are rewritten.
	clientClose := req.Close
	clientMajor, clientMinor := req.ProtoMajor, req.ProtoMinor
	upgradeProto, upgrade := isUpgrade(req.Header)

	removeHopByHop(req.Header)
	u.User = nil
	req.RequestURI = ""
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", upgradeProto)
	} else {
		// A fresh upstream connection per request, closed after the answer.
		req.Close = true
	}
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		// We do not forward the expectation; answer it so the browser sends the
		// body.
		_, _ = io.WriteString(s.client, "HTTP/1.1 100 Continue\r\n\r\n")
		req.Header.Del("Expect")
	}
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""} // do not let net/http invent one
	}

	var capped atomic.Bool
	bodyless := req.Body == nil || req.Body == http.NoBody
	if !bodyless {
		req.Body = &cappedBody{rc: req.Body, limit: p.cfg.MaxTunnelBytes, counter: p.m.bytesUp, capped: &capped}
	}

	res, fail := s.exchange(req, t, bodyless)
	if fail != nil {
		result := fail.result
		if capped.Load() {
			result = resByteCap
		}
		p.m.result(kindHTTP, result)
		s.reply(fail.status, fail.msg)
		return false
	}
	resp := res.resp

	// Rewrite the response head for the browser. Response.Write emits many
	// small writes; coalesce them.
	bw := bufio.NewWriterSize(s.client, 8192)
	// Hide bufio's ReaderFrom: io.Copy would otherwise read the body straight
	// into the writer's buffer, and the flush in cappedBody.beforeRead would
	// then reset that buffer under the read in flight.
	out := writerOnly{bw}
	upProto := resp.Header.Get("Upgrade")
	// Whether the browser connection survives depends on the browser's wish and
	// on the response being delimited, not on the upstream hop (we asked the
	// upstream to close after one exchange).
	keep := !clientClose
	removeHopByHop(resp.Header)
	resp.ProtoMajor, resp.ProtoMinor = clientMajor, clientMinor

	if resp.StatusCode == http.StatusSwitchingProtocols {
		if !upgrade {
			s.detach()
			p.m.result(kindHTTP, resUpstreamError)
			s.reply(http.StatusBadGateway, "egress proxy: upstream switched protocols unasked")
			return false
		}
		resp.Header.Set("Connection", "Upgrade")
		resp.Header.Set("Upgrade", upProto)
		err := resp.Write(out)
		if err == nil {
			err = bw.Flush()
		}
		if err != nil {
			s.detach()
			p.m.result(kindHTTP, resUpstreamError)
			return false
		}
		p.m.duration.WithLabelValues(kindHTTP).Observe(time.Since(start).Seconds())
		result := resOK
		if s.splice(res.conn, s.br, res.br) {
			result = resByteCap
		}
		p.m.result(kindHTTP, result)
		return false
	}

	// An unframed body can only be delimited by closing the connection.
	if resp.ContentLength < 0 && !(len(resp.TransferEncoding) > 0 && resp.TransferEncoding[0] == "chunked") {
		keep = false
	}
	resp.Close = !keep
	resp.Body = &cappedBody{rc: resp.Body, limit: p.cfg.MaxTunnelBytes, counter: p.m.bytesDown, capped: &capped,
		// Send what is buffered before waiting for the origin, so a streamed
		// response is not held back by the write buffer.
		beforeRead: func() { _ = bw.Flush() }}
	err := resp.Write(out)
	if err == nil {
		err = bw.Flush()
	}
	s.detach()
	if err != nil {
		result := resUpstreamError
		if capped.Load() {
			result = resByteCap
		}
		p.m.result(kindHTTP, result)
		return false
	}
	p.m.duration.WithLabelValues(kindHTTP).Observe(time.Since(start).Seconds())
	p.m.result(kindHTTP, resOK)
	return keep
}

// clientBodyFailed reports that reading the browser's request body failed.
func clientBodyFailed(req *http.Request) bool {
	cb, ok := req.Body.(*cappedBody)
	return ok && cb.readErr != nil
}

// exchange sends req through an upstream and returns the first usable
// response head: at most maxAttempts attempts inside DialTimeout, the second
// on a different upstream when possible. A request is retried only when it is
// safe: a dial failure (nothing was sent), or a body-less GET/HEAD/OPTIONS
// whose exchange failed or was refused with 407.
func (s *session) exchange(req *http.Request, t Target, bodyless bool) (forwardResult, *failure) {
	p := s.p
	start := time.Now()
	deadline := start.Add(p.cfg.DialTimeout)
	idempotent := bodyless && (req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions)
	var last *failure
	avoid := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if p.ctx.Err() != nil {
			return forwardResult{}, &failure{http.StatusServiceUnavailable, resUpstreamError, "egress proxy: shutting down"}
		}
		if !time.Now().Before(deadline) {
			break
		}
		up, err := p.pickUpstream(t, attempt, avoid)
		if err != nil {
			if last != nil {
				break
			}
			return forwardResult{}, p.noUpstream(err)
		}
		avoid = up.addr
		ctx, cancel := p.attemptContext(p.ctx, attempt, start, deadline)
		res := s.forwardVia(ctx, req, up)
		cancel()
		if p.ctx.Err() != nil {
			s.detach()
			return forwardResult{}, &failure{http.StatusServiceUnavailable, resUpstreamError, "egress proxy: shutting down"}
		}

		retriable := false
		switch {
		case res.err != nil && clientBodyFailed(req):
			// The browser's own request body failed (it went away mid-upload, or
			// hit the byte cap): that says nothing about the upstream, so no
			// verdict on the exit IP and no retry.
			s.detach()
			status, msg := http.StatusBadRequest, "egress proxy: request body failed"
			if errors.Is(req.Body.(*cappedBody).readErr, errByteCap) {
				status, msg = http.StatusRequestEntityTooLarge, "egress proxy: request body over the byte cap"
			}
			return forwardResult{}, &failure{status, resUpstreamError, msg}
		case res.err != nil:
			att := attHandshakeError
			if res.stage == "dial" {
				att = attDialError
				retriable = true
			} else {
				retriable = idempotent
			}
			p.m.attempts.WithLabelValues(att).Inc()
			p.markResult(up.addr, false)
			p.warn(att, "upstream attempt failed", "upstream", up.addr, "scheme", up.scheme, "result", att, "attempt", attempt, "error", res.err.Error())
			last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: upstream proxy unreachable"}
		case res.resp.StatusCode == http.StatusProxyAuthRequired:
			s.detach() // closes the connection; the body is not worth draining
			retriable = bodyless
			p.m.attempts.WithLabelValues(attAuthFailed).Inc()
			p.markResult(up.addr, false)
			p.warn(attAuthFailed, "upstream proxy refused our credentials", "upstream", up.addr, "scheme", up.scheme, "result", attAuthFailed, "attempt", attempt)
			last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: upstream proxy refused the credentials"}
		default:
			p.m.attempts.WithLabelValues(attOK).Inc()
			p.markResult(up.addr, true)
			return res, nil
		}
		if !retriable {
			break
		}
	}
	if last == nil {
		last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: no upstream attempt completed in time"}
	}
	return forwardResult{}, last
}

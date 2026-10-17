package egressproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// failure is a request that could not be served, with the answer to give.
type failure struct {
	status int
	result string // requests_total result label
	msg    string
}

// splitAuthority parses a CONNECT target "host:port" (IPv6 literals in
// brackets). The port is mandatory.
func splitAuthority(hostport string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(hostport)
	if err != nil {
		return "", "", err
	}
	if err := checkTarget(host, port); err != nil {
		return "", "", err
	}
	return host, port, nil
}

var errBadTarget = errors.New("bad target")

// checkTarget rejects empty or oddly shaped hosts and out-of-range ports before
// they are used to build a request line for the upstream.
func checkTarget(host, port string) error {
	if host == "" || strings.ContainsAny(host, " \t\r\n\x00/\\@") {
		return errBadTarget
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errBadTarget
	}
	return nil
}

// checkGuard runs the SSRF guard (cached) for t. It writes the refusal and
// counts the result itself, and reports whether the request may go on.
func (s *session) checkGuard(t Target) bool {
	p := s.p
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.DialTimeout)
	defer cancel()
	err := p.guard.check(ctx, p.cfg.Guard, t.Host, t.Port)
	if err == nil {
		return true
	}
	target := net.JoinHostPort(t.Host, t.Port)
	if errors.Is(err, ErrGuardUnavailable) {
		p.m.result(t.Kind, resGuardError)
		p.warn("guard_error", "guard could not verify target, refusing", "kind", t.Kind, "target", target, "error", err.Error())
		s.reply(http.StatusBadGateway, "egress proxy: target could not be verified")
		return false
	}
	p.m.result(t.Kind, resGuardDenied)
	p.warn("guard_denied", "target denied by guard", "kind", t.Kind, "target", target, "error", err.Error())
	s.reply(http.StatusForbidden, "egress proxy: target not permitted")
	return false
}

// handleConnect serves one CONNECT request: guard, pick an upstream, ask it
// for a tunnel (injecting Proxy-Authorization) and splice. The connection
// ends with the tunnel.
func (s *session) handleConnect(req *http.Request) {
	p := s.p
	start := time.Now()
	host, port, err := splitAuthority(req.Host)
	if err != nil {
		p.m.result(kindConnect, resBadRequest)
		s.reply(http.StatusBadRequest, "egress proxy: CONNECT needs host:port")
		return
	}
	t := Target{Kind: kindConnect, Host: host, Port: port, Client: s.raw.RemoteAddr()}
	if !s.checkGuard(t) {
		return
	}
	out, fail := s.establish(t, net.JoinHostPort(host, port))
	if fail != nil {
		p.m.result(kindConnect, fail.result)
		s.reply(fail.status, fail.msg)
		return
	}
	if _, err := io.WriteString(s.client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	p.m.duration.WithLabelValues(kindConnect).Observe(time.Since(start).Seconds())
	result := resOK
	if s.splice(out.conn, s.br, out.br) {
		result = resByteCap
		p.warn("byte_cap", "tunnel closed at the byte cap", "max_tunnel_bytes", p.cfg.MaxTunnelBytes)
	}
	p.m.result(kindConnect, result)
}

// establish opens a tunnel to authority through an upstream proxy: at most
// maxAttempts attempts, the second on a different upstream when possible, all
// inside DialTimeout, and only while nothing has been relayed to the browser.
func (s *session) establish(t Target, authority string) (connectOutcome, *failure) {
	p := s.p
	start := time.Now()
	deadline := start.Add(p.cfg.DialTimeout)
	var last *failure
	avoid := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if p.ctx.Err() != nil {
			return connectOutcome{}, &failure{http.StatusServiceUnavailable, resUpstreamError, "egress proxy: shutting down"}
		}
		if !time.Now().Before(deadline) {
			break
		}
		up, err := p.pickUpstream(t, attempt, avoid)
		if err != nil {
			if last != nil {
				break // report the concrete failure of the attempt before
			}
			return connectOutcome{}, p.noUpstream(err)
		}
		avoid = up.addr
		ctx, cancel := p.attemptContext(p.ctx, attempt, start, deadline)
		out := s.connectVia(ctx, up, authority)
		cancel()
		if p.ctx.Err() != nil {
			// Shutting down: the attempt was cut short by us, which says
			// nothing about the exit IP, so no verdict is reported.
			s.detach()
			return connectOutcome{}, &failure{http.StatusServiceUnavailable, resUpstreamError, "egress proxy: shutting down"}
		}

		switch {
		case out.err != nil:
			att := attHandshakeError
			if out.dialFailed {
				att = attDialError
			}
			p.m.attempts.WithLabelValues(att).Inc()
			p.markResult(up.addr, false)
			p.warn(att, "upstream attempt failed", "upstream", up.addr, "scheme", up.scheme, "result", att, "attempt", attempt, "error", out.err.Error())
			last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: upstream proxy unreachable"}
		case out.status/100 == 2:
			p.m.attempts.WithLabelValues(attOK).Inc()
			p.markResult(up.addr, true)
			return out, nil
		case out.status == http.StatusProxyAuthRequired:
			s.detach()
			p.m.attempts.WithLabelValues(attAuthFailed).Inc()
			p.markResult(up.addr, false)
			p.warn(attAuthFailed, "upstream proxy refused our credentials", "upstream", up.addr, "scheme", up.scheme, "result", attAuthFailed, "attempt", attempt)
			last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: upstream proxy refused the credentials"}
		case out.status >= 500 && !p.cfg.IgnoreUpstream5xx:
			s.detach()
			p.m.attempts.WithLabelValues(attUpstream5xx).Inc()
			p.markResult(up.addr, false)
			p.warn(attUpstream5xx, "upstream proxy answered 5xx to CONNECT", "upstream", up.addr, "scheme", up.scheme, "result", attUpstream5xx, "attempt", attempt, "status", out.status)
			last = &failure{relayStatus(out.status), resUpstreamError, "egress proxy: upstream proxy answered " + strconv.Itoa(out.status)}
		default:
			// A 4xx the upstream gave for this target (or a 5xx we were told to
			// pass on): relay it, no retry, no verdict on the exit IP.
			s.detach()
			p.m.attempts.WithLabelValues(attRejected).Inc()
			return connectOutcome{}, &failure{relayStatus(out.status), resUpstreamError, "egress proxy: upstream proxy answered " + strconv.Itoa(out.status)}
		}
	}
	if last == nil {
		last = &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: no upstream attempt completed in time"}
	}
	return connectOutcome{}, last
}

// noUpstream builds the fail-closed answer for "nothing to send this through".
func (p *Proxy) noUpstream(err error) *failure {
	if errors.Is(err, errBadUpstream) {
		p.warn("bad_upstream", "supplier returned an unusable upstream proxy URL (expected http://[user:pass@]host:port)")
		return &failure{http.StatusBadGateway, resUpstreamError, "egress proxy: upstream proxy configuration unusable"}
	}
	p.warn("no_upstream", "no upstream proxy available, refusing (fail closed)")
	return &failure{http.StatusServiceUnavailable, resNoUpstream, "egress proxy: no upstream proxy available (egress is required, refusing to connect directly)"}
}

// relayStatus is the status to give the browser for an upstream's refusal:
// the upstream's own 4xx/5xx, except 407 (which the browser could only fail
// to answer) and anything that is not an error status.
func relayStatus(code int) int {
	if code >= 400 && code < 600 && code != http.StatusProxyAuthRequired {
		return code
	}
	return http.StatusBadGateway
}

package egressproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxHeaderBytes bounds one request's header block, like net/http's default.
const maxHeaderBytes = 1 << 20

// Proxy is a running local forward proxy. Create it with Start and stop it with
// Close.
type Proxy struct {
	cfg  Config
	log  *slog.Logger
	m    *metrics
	ln   net.Listener
	addr string

	guard *guardCache
	logs  *logLimiter

	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool // detaches the parent-context watcher
	sem    chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	sessions map[*session]struct{}

	closeOnce sync.Once
}

// Start listens on 127.0.0.1 on an ephemeral port and serves until ctx is
// cancelled or Close is called. It never listens on any other interface.
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.RequireUpstream && cfg.Supplier == nil {
		return nil, ErrNoSupplier
	}
	cfg = cfg.withDefaults()
	m, err := newMetrics(cfg.Registerer)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egressproxy: listen: %w", err)
	}
	p := &Proxy{
		cfg:      cfg,
		log:      cfg.Logger.With("component", "egressproxy"),
		m:        m,
		ln:       ln,
		addr:     ln.Addr().String(),
		guard:    newGuardCache(cfg.GuardCacheTTL, cfg.GuardCacheSize, cfg.Now),
		logs:     newLogLimiter(cfg.Now),
		sem:      make(chan struct{}, cfg.MaxConns),
		sessions: make(map[*session]struct{}),
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.wg.Add(1)
	go p.acceptLoop()
	// Registered after the accept loop is counted, so a Close triggered by an
	// already racing cancellation waits for it instead of missing it.
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	p.mu.Lock()
	p.stop = stop
	p.mu.Unlock()
	p.log.Info("egress proxy listening", "addr", p.addr)
	return p, nil
}

// Addr returns the listen address, "127.0.0.1:PORT".
func (p *Proxy) Addr() string { return p.addr }

// URL returns the proxy URL to hand to a browser, "http://127.0.0.1:PORT".
func (p *Proxy) URL() string { return "http://" + p.addr }

// Close stops accepting, closes every open connection and tunnel and waits for
// the handlers to finish. It is safe to call more than once.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		stop := p.stop
		p.closed = true
		sessions := make([]*session, 0, len(p.sessions))
		for s := range p.sessions {
			sessions = append(sessions, s)
		}
		p.mu.Unlock()
		if stop != nil {
			stop()
		}
		p.cancel()
		_ = p.ln.Close()
		for _, s := range sessions {
			s.closeAll()
		}
		p.wg.Wait()
	})
	return nil
}

func (p *Proxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	var backoff time.Duration
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if p.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			// EMFILE and friends: wait instead of spinning.
			backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
			p.warn("accept", "accept failed", "error", err.Error())
			select {
			case <-time.After(backoff):
				continue
			case <-p.ctx.Done():
				return
			}
		}
		backoff = 0
		select {
		case p.sem <- struct{}{}:
		default:
			p.refuseOverload(c)
			continue
		}
		s := newSession(p, c)
		if !p.addSession(s) {
			<-p.sem
			_ = c.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.sem }()
			defer p.removeSession(s)
			s.serve()
		}()
	}
}

func (p *Proxy) addSession(s *session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.sessions[s] = struct{}{}
	return true
}

func (p *Proxy) removeSession(s *session) {
	p.mu.Lock()
	delete(p.sessions, s)
	p.mu.Unlock()
}

// refuseOverload answers a connection over MaxConns with a fast 503. The write
// is tiny and bounded so a slow client cannot stall the accept loop.
func (p *Proxy) refuseOverload(c net.Conn) {
	p.m.result(kindUnknown, resOverload)
	p.warn("overload", "connection limit reached, refusing", "max_conns", p.cfg.MaxConns)
	_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = io.WriteString(c, errorResponse(http.StatusServiceUnavailable, "egress proxy: too many connections"))
	_ = c.Close()
}

// errorResponse renders a complete close-delimited error response.
func errorResponse(status int, msg string) string {
	body := msg + "\n"
	retry := ""
	if status == http.StatusServiceUnavailable {
		retry = "Retry-After: 1\r\n"
	}
	return "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		retry + "\r\n" + body
}

// warn logs at warn level at most once per 10 s per key, counting what it
// suppressed, so a failing upstream cannot flood the service log.
func (p *Proxy) warn(key, msg string, args ...any) {
	if ok, suppressed := p.logs.allow(key); ok {
		if suppressed > 0 {
			args = append(args, "suppressed", suppressed)
		}
		p.log.Warn(msg, args...)
	}
}

// logLimiter admits one log per key per interval.
type logLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
	supp map[string]int
}

const logInterval = 10 * time.Second

func newLogLimiter(now func() time.Time) *logLimiter {
	return &logLimiter{now: now, last: map[string]time.Time{}, supp: map[string]int{}}
}

func (l *logLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < logInterval {
		l.supp[key]++
		return false, 0
	}
	l.last[key] = now
	n := l.supp[key]
	l.supp[key] = 0
	return true, n
}

// session is one accepted client connection and the upstream connection it
// currently drives. A single idle watchdog covers both, so a request waiting on
// a slow origin and a tunnel with traffic in only one direction are both judged
// by "did anything move".
type session struct {
	p      *Proxy
	raw    net.Conn
	client *activityConn
	br     *bufio.Reader
	hdr    *limitedReader
	wd     *watchdog

	mu     sync.Mutex
	up     net.Conn
	closed bool
}

func newSession(p *Proxy, raw net.Conn) *session {
	s := &session{p: p, raw: raw}
	s.wd = newWatchdog(p.cfg.IdleTimeout, s.closeAll)
	s.client = &activityConn{Conn: raw, wd: s.wd}
	s.hdr = &limitedReader{r: s.client, n: maxHeaderBytes}
	s.br = bufio.NewReaderSize(s.hdr, 4096)
	return s
}

// closeAll closes the client and the current upstream connection. It is the
// watchdog's action, Proxy.Close's action and the end of every handler.
func (s *session) closeAll() {
	s.mu.Lock()
	s.closed = true
	up := s.up
	s.up = nil
	s.mu.Unlock()
	_ = s.raw.Close()
	if up != nil {
		_ = up.Close()
	}
}

// attach records the upstream connection so closeAll can reach it. It reports
// false (and closes conn) when the session is already closed.
func (s *session) attach(conn net.Conn) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return false
	}
	s.up = conn
	s.mu.Unlock()
	return true
}

// detach closes and forgets the upstream connection but keeps the client.
func (s *session) detach() {
	s.mu.Lock()
	up := s.up
	s.up = nil
	s.mu.Unlock()
	if up != nil {
		_ = up.Close()
	}
}

func (s *session) serve() {
	defer s.wd.stop()
	defer s.closeAll()
	first := true
	for {
		// Wait for the first byte of a request. A new connection must speak
		// within ReadHeaderTimeout; a kept-alive one may idle (the watchdog
		// bounds that).
		if first {
			_ = s.raw.SetReadDeadline(time.Now().Add(s.p.cfg.ReadHeaderTimeout))
		} else {
			_ = s.raw.SetReadDeadline(time.Time{})
		}
		if _, err := s.br.Peek(1); err != nil {
			return
		}
		first = false
		_ = s.raw.SetReadDeadline(time.Now().Add(s.p.cfg.ReadHeaderTimeout))
		s.hdr.n, s.hdr.err = maxHeaderBytes+4096, nil
		req, err := http.ReadRequest(s.br)
		s.hdr.n = unlimited
		_ = s.raw.SetReadDeadline(time.Time{})
		if err != nil {
			switch {
			case errors.Is(s.hdr.err, errHeaderTooLarge):
				s.p.m.result(kindUnknown, resBadRequest)
				s.reply(http.StatusRequestHeaderFieldsTooLarge, "egress proxy: request header too large")
			case s.hdr.err != nil:
				// The connection itself failed or the client was too slow: a
				// reply would go nowhere. (bufio hands a half-read line to the
				// parser as if it were complete, so the parse error is not the
				// cause here.)
			default:
				s.p.m.result(kindUnknown, resBadRequest)
				s.reply(http.StatusBadRequest, "egress proxy: malformed request")
			}
			return
		}
		switch {
		case req.Method == http.MethodConnect:
			s.handleConnect(req)
			return
		case req.URL != nil && req.URL.IsAbs():
			if !s.handleForward(req) {
				return
			}
		default:
			s.p.m.result(kindUnknown, resBadRequest)
			s.reply(http.StatusBadRequest, "egress proxy: only CONNECT and absolute-URI requests are served")
			return
		}
	}
}

// reply writes a complete error response and nothing else. The caller closes
// the connection afterwards.
func (s *session) reply(status int, msg string) {
	_ = s.raw.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(s.client, errorResponse(status, msg))
	_ = s.raw.SetWriteDeadline(time.Time{})
}

const unlimited = int64(1<<63 - 1)

// limitedReader caps the bytes read from r. The session re-arms it before each
// request head and lifts it afterwards, so the header limit does not apply to
// bodies or tunnels.
type limitedReader struct {
	r   io.Reader
	n   int64
	err error // the last error the underlying reader returned
}

var errHeaderTooLarge = errors.New("egressproxy: request header too large")

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		l.err = errHeaderTooLarge
		return 0, errHeaderTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if err != nil {
		l.err = err
	}
	return n, err
}

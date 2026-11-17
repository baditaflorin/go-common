// Package proxybudget meters proxy socket bytes against a durable, bounded
// account. It is independent of provider selection, authentication, destination
// allowlists and the fleet token ledger; callers must enforce those separately.
// The accounting and reservations are extracted from go-search-duck-go's lane.
package proxybudget

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrConfig      = errors.New("proxybudget: invalid bounded configuration")
	ErrUnavailable = errors.New("proxybudget: ledger unavailable")
	ErrClosed      = errors.New("proxybudget: closed")
	ErrDisabled    = errors.New("proxybudget: disabled")
	ErrBusy        = errors.New("proxybudget: in-flight limit reached")
	ErrBudget      = errors.New("proxybudget: total budget exhausted")
	ErrDaily       = errors.New("proxybudget: daily budget exhausted")
	ErrRequest     = errors.New("proxybudget: request byte cap reached")
)

// Config requires positive total, daily, per-request and concurrency limits.
// StateDir must be a private, persistent absolute path unique to this account
// allocation. Never open another process's existing lane ledger here.
type Config struct {
	StateDir         string        `json:"state_dir"`
	TotalBudgetBytes int64         `json:"total_budget_bytes"`
	DailyCapBytes    int64         `json:"daily_cap_bytes"`
	MaxRequestBytes  int64         `json:"max_request_bytes"`
	MaxInflight      int           `json:"max_inflight"`
	InitialUsedBytes int64         `json:"initial_used_bytes,omitempty"`
	FlushInterval    time.Duration `json:"flush_interval,omitempty"`
}

// Manager owns a locked durable byte ledger and the live request tickets.
type Manager struct {
	cfg       Config
	ledger    *byteLedger
	mu        sync.Mutex
	tickets   map[*Ticket]struct{}
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// Open validates limits and fails closed on a missing permission, corrupt
// ledger or an already-owned state directory. Existing usage is never reset.
func Open(cfg Config) (*Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l, err := openByteLedger(cfg.StateDir, cfg.InitialUsedBytes, cfg.FlushInterval, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &Manager{cfg: cfg, ledger: l, tickets: make(map[*Ticket]struct{})}, nil
}

func (cfg Config) validate() error {
	if !filepath.IsAbs(cfg.StateDir) || cfg.StateDir == string(filepath.Separator) ||
		cfg.TotalBudgetBytes <= 0 || cfg.TotalBudgetBytes > math.MaxInt64/4 ||
		cfg.DailyCapBytes <= 0 || cfg.DailyCapBytes > cfg.TotalBudgetBytes ||
		cfg.MaxRequestBytes <= 0 || cfg.MaxRequestBytes > cfg.DailyCapBytes ||
		cfg.MaxInflight <= 0 || cfg.MaxInflight > 4096 || cfg.InitialUsedBytes < 0 ||
		cfg.InitialUsedBytes > math.MaxInt64/4 || cfg.FlushInterval < 0 {
		return ErrConfig
	}
	return nil
}

func (m *Manager) enabled() error {
	_, err := os.Stat(filepath.Join(m.cfg.StateDir, "DISABLED"))
	if err == nil {
		return ErrDisabled
	}
	if !errors.Is(err, os.ErrNotExist) || m.ledger.failed.Load() {
		return ErrUnavailable
	}
	return nil
}

// Admit reserves the full per-request cap before any upstream socket exists.
// Cancellation closes the ticket's sockets and releases unused reservations.
func (m *Manager) Admit(ctx context.Context) (*Ticket, error) {
	if m == nil || ctx == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if err := m.enabled(); err != nil {
		return nil, err
	}
	if len(m.tickets) >= m.cfg.MaxInflight {
		return nil, ErrBusy
	}
	ok, reason := m.ledger.Reserve(m.cfg.MaxRequestBytes, m.cfg.TotalBudgetBytes, m.cfg.DailyCapBytes)
	if !ok {
		if reason == "daily_cap" {
			return nil, ErrDaily
		}
		return nil, ErrBudget
	}
	t := &Ticket{manager: m, ctx: ctx, conns: make(map[*countingConn]struct{})}
	t.reserved.Store(m.cfg.MaxRequestBytes)
	m.tickets[t] = struct{}{}
	m.ledger.AddRequest()
	// Persist the held maximum before admitting a socket. A crash recovers
	// this allowance as spent, so repeated crashes cannot refill the budget.
	if err := m.ledger.Flush(); err != nil {
		delete(m.tickets, t)
		m.ledger.Unreserve(t.reserved.Swap(0))
		return nil, ErrUnavailable
	}
	t.mu.Lock()
	t.stopContext = context.AfterFunc(ctx, func() { _ = t.Close() })
	t.mu.Unlock()
	return t, nil
}

// Status contains only byte counts and limits, never upstream credentials.
type Status struct {
	TotalUsedBytes   int64 `json:"total_used_bytes"`
	DailyUsedBytes   int64 `json:"daily_used_bytes"`
	ReservedBytes    int64 `json:"reserved_bytes"`
	TotalBudgetBytes int64 `json:"total_budget_bytes"`
	DailyCapBytes    int64 `json:"daily_cap_bytes"`
	MaxRequestBytes  int64 `json:"max_request_bytes"`
	Inflight         int   `json:"inflight"`
	Disabled         bool  `json:"disabled"`
	Closed           bool  `json:"closed"`
}

func (m *Manager) Status() Status {
	if m == nil {
		return Status{Closed: true, Disabled: true}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.ledger.byteSnapshot()
	return Status{TotalUsedBytes: s.Total, DailyUsedBytes: s.Day, ReservedBytes: s.Reserved,
		TotalBudgetBytes: m.cfg.TotalBudgetBytes, DailyCapBytes: m.cfg.DailyCapBytes,
		MaxRequestBytes: m.cfg.MaxRequestBytes, Inflight: len(m.tickets),
		Closed: m.closed, Disabled: m.enabled() != nil}
}

// Close rejects admissions, drains every owned socket, then flushes and unlocks
// the ledger. This ordering prevents post-flush TLS bytes being lost on shutdown.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		tickets := make([]*Ticket, 0, len(m.tickets))
		for t := range m.tickets {
			tickets = append(tickets, t)
		}
		m.mu.Unlock()
		for _, t := range tickets {
			if err := t.Close(); err != nil {
				m.closeErr = err
			}
		}
		if err := m.ledger.Close(); err != nil {
			m.closeErr = ErrUnavailable
		}
	})
	return m.closeErr
}

// Ticket is a single admitted request across all its proxy connections. It is
// safe to wrap multiple browser subresource sockets in the same ticket.
type Ticket struct {
	manager                      *Manager
	ctx                          context.Context
	mu                           sync.Mutex
	closed                       bool
	conns                        map[*countingConn]struct{}
	ops                          sync.WaitGroup
	in, out, allocated, reserved atomic.Int64
	closeOnce                    sync.Once
	closeErr                     error
	stopContext                  func() bool
}

// Usage is the actual socket usage recorded so far, in both directions.
type Usage struct {
	InBytes    int64 `json:"in_bytes"`
	OutBytes   int64 `json:"out_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

func (t *Ticket) Usage() Usage {
	if t == nil {
		return Usage{}
	}
	in, out := t.in.Load(), t.out.Load()
	return Usage{in, out, in + out}
}

// WrapConn takes ownership of a raw upstream TCP socket before proxy TLS or
// CONNECT. It never dials or chooses an address, so the caller's SSRF guards
// remain in force. A refused wrapper closes the supplied connection.
func (t *Ticket) WrapConn(conn net.Conn) (net.Conn, error) {
	if conn == nil {
		return nil, ErrUnavailable
	}
	if t == nil {
		_ = conn.Close()
		return nil, ErrClosed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = conn.Close()
		return nil, ErrClosed
	}
	if err := t.ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := t.manager.enabled(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := &countingConn{Conn: conn, ticket: t}
	t.conns[c] = struct{}{}
	return c, nil
}

func (t *Ticket) begin() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if err := t.manager.enabled(); err != nil {
		return err
	}
	t.ops.Add(1)
	return nil
}

func (t *Ticket) claim(want int) int64 {
	for {
		// One atomic counter holds consumed bytes plus outstanding I/O claims.
		// Moving a completed claim into the usage counters must not count it
		// twice temporarily and refuse unrelated connections before the cap.
		allocated := t.allocated.Load()
		remaining := t.manager.cfg.MaxRequestBytes - allocated
		if remaining <= 0 {
			return 0
		}
		n := min(int64(want), remaining)
		if t.allocated.CompareAndSwap(allocated, allocated+n) {
			return n
		}
	}
}

func (t *Ticket) consume(n int64, read bool) {
	if n <= 0 {
		return
	}
	if read {
		t.in.Add(n)
	} else {
		t.out.Add(n)
	}
	t.reserved.Add(-n)
	t.manager.ledger.settleBytes(n, read)
}

// Close is idempotent. No new I/O can start after the close barrier; outstanding
// reads/writes are unblocked by closing their sockets, then fully accounted.
func (t *Ticket) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		conns := make([]*countingConn, 0, len(t.conns))
		for c := range t.conns {
			conns = append(conns, c)
		}
		t.mu.Unlock()
		if t.stopContext != nil {
			t.stopContext()
		}
		for _, c := range conns {
			_ = c.Close()
		}
		t.ops.Wait()
		t.manager.ledger.Unreserve(t.reserved.Swap(0))
		t.manager.mu.Lock()
		delete(t.manager.tickets, t)
		t.manager.mu.Unlock()
		if err := t.manager.ledger.Flush(); err != nil {
			t.closeErr = ErrUnavailable
		}
	})
	return t.closeErr
}

type countingConn struct {
	net.Conn
	ticket    *Ticket
	closeOnce sync.Once
	closeErr  error
}

func (c *countingConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		c.ticket.mu.Lock()
		delete(c.ticket.conns, c)
		c.ticket.mu.Unlock()
	})
	return c.closeErr
}
func (c *countingConn) Read(p []byte) (int, error)  { return c.transfer(p, true) }
func (c *countingConn) Write(p []byte) (int, error) { return c.transfer(p, false) }
func (c *countingConn) transfer(p []byte, read bool) (int, error) {
	if err := c.ticket.begin(); err != nil {
		_ = c.Close()
		return 0, err
	}
	defer c.ticket.ops.Done()
	if len(p) == 0 {
		return 0, nil
	}
	claimed := c.ticket.claim(len(p))
	if claimed == 0 {
		_ = c.Close()
		return 0, ErrRequest
	}
	var n int
	// Consumed bytes remain allocated for the ticket's lifetime. Only the
	// unused portion of this I/O claim becomes available to another socket.
	defer func() { c.ticket.allocated.Add(int64(n) - claimed) }()
	want := len(p)
	p = p[:int(claimed)]
	var err error
	if read {
		n, err = c.Conn.Read(p)
	} else {
		n, err = c.Conn.Write(p)
	}
	c.ticket.consume(int64(n), read)
	if claimed < int64(want) && n == len(p) && err == nil {
		_ = c.Close()
		err = ErrRequest
	}
	return n, err
}

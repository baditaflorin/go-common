package proxybudget

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func configFor(t *testing.T) Config {
	t.Helper()
	return Config{StateDir: t.TempDir(), TotalBudgetBytes: 1000, DailyCapBytes: 500, MaxRequestBytes: 100, MaxInflight: 16, FlushInterval: time.Hour}
}
func openFor(t *testing.T, c Config) *Manager {
	t.Helper()
	m, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func admitFor(t *testing.T, m *Manager) *Ticket {
	t.Helper()
	tk, err := m.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tk.Close() })
	return tk
}

type memoryConn struct{ closed atomic.Bool }

func (c *memoryConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func (c *memoryConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return len(p), nil
}
func (c *memoryConn) Close() error                   { c.closed.Store(true); return nil }
func (*memoryConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*memoryConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }

func TestUsagePersistsAndInitialSeedNeverResetsIt(t *testing.T) {
	c := configFor(t)
	c.InitialUsedBytes = 75
	m := openFor(t, c)
	tk := admitFor(t, m)
	conn, err := tk.WrapConn(&memoryConn{})
	if err != nil {
		t.Fatal(err)
	}
	if n, e := conn.Write(make([]byte, 17)); n != 17 || e != nil {
		t.Fatal(n, e)
	}
	if n, e := conn.Read(make([]byte, 23)); n != 23 || e != nil {
		t.Fatal(n, e)
	}
	if u := tk.Usage(); u.InBytes != 23 || u.OutBytes != 17 || u.TotalBytes != 40 {
		t.Fatal(u)
	}
	if err = tk.Close(); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.TotalUsedBytes != 115 || s.DailyUsedBytes != 40 || s.ReservedBytes != 0 || s.Inflight != 0 {
		t.Fatal(s)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	c.InitialUsedBytes = 600
	m2 := openFor(t, c)
	if s := m2.Status(); s.TotalUsedBytes != 115 {
		t.Fatal("seed replaced existing bytes", s)
	}
	if _, err = m.Admit(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestConcurrentAdmissionsReserveBeforeAnySocket(t *testing.T) {
	c := configFor(t)
	c.TotalBudgetBytes = 200
	c.DailyCapBytes = 200
	c.MaxRequestBytes = 50
	c.MaxInflight = 64
	m := openFor(t, c)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var tickets []*Ticket
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, err := m.Admit(context.Background())
			if err == nil {
				mu.Lock()
				tickets = append(tickets, tk)
				mu.Unlock()
			} else if !errors.Is(err, ErrBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(tickets) != 4 || m.Status().ReservedBytes != 200 {
		t.Fatal(len(tickets), m.Status())
	}
	for _, tk := range tickets {
		if err := tk.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if m.Status().ReservedBytes != 0 {
		t.Fatal(m.Status())
	}
}

func TestOneRequestCapAcrossConcurrentConnectionsAndDirections(t *testing.T) {
	m := openFor(t, configFor(t))
	tk := admitFor(t, m)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		conn, err := tk.WrapConn(&memoryConn{})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := make([]byte, 13)
			if i%2 == 0 {
				_, _ = conn.Read(b)
			} else {
				_, _ = conn.Write(b)
			}
		}(i)
	}
	wg.Wait()
	if u := tk.Usage(); u.TotalBytes != 100 {
		t.Fatal("combined cap was not exact", u)
	}
	if err := tk.Close(); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.TotalUsedBytes != 100 || s.ReservedBytes != 0 {
		t.Fatal(s)
	}
}

func TestCloseDrainsBlockedSocketBeforeFlushing(t *testing.T) {
	m := openFor(t, configFor(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk, err := m.Admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, peer := net.Pipe()
	defer peer.Close()
	conn, err := tk.WrapConn(raw)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { defer close(readDone); _, _ = conn.Read(make([]byte, 20)) }()
	if _, err = peer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	<-readDone
	readDone = make(chan struct{})
	go func() { defer close(readDone); _, _ = conn.Read(make([]byte, 20)) }()
	cancel()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock socket")
	}
	if err = tk.Close(); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.TotalUsedBytes != 5 || s.ReservedBytes != 0 {
		t.Fatal(s)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	var file ledgerFile
	b, _ := os.ReadFile(filepath.Join(m.cfg.StateDir, "ledger.json"))
	if err = json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	if file.TotalIn != 5 {
		t.Fatal("last bytes not durable", file)
	}
	refused := &memoryConn{}
	if _, err = tk.WrapConn(refused); !errors.Is(err, ErrClosed) || !refused.closed.Load() {
		t.Fatal("closed ticket admitted socket", err)
	}
}

func TestLimitsKillSwitchAndZeroLengthIO(t *testing.T) {
	c := configFor(t)
	c.MaxInflight = 1
	m := openFor(t, c)
	tk := admitFor(t, m)
	if _, err := m.Admit(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	conn, _ := tk.WrapConn(&memoryConn{})
	if n, err := conn.Write(make([]byte, 100)); n != 100 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := conn.Read(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := conn.Write(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := conn.Read([]byte{0}); n != 0 || !errors.Is(err, ErrRequest) {
		t.Fatal(n, err)
	}
	_ = tk.Close()
	kill := filepath.Join(c.StateDir, "DISABLED")
	if err := os.WriteFile(kill, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Admit(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if !m.Status().Disabled {
		t.Fatal("kill switch missing from status")
	}
	_ = os.Remove(kill)
	tk = admitFor(t, m)
	conn, _ = tk.WrapConn(&memoryConn{})
	_ = os.WriteFile(kill, nil, 0600)
	if _, err := conn.Write([]byte("x")); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
}

func TestBudgetAndDailyRefusals(t *testing.T) {
	for _, daily := range []bool{false, true} {
		t.Run(map[bool]string{false: "total", true: "daily"}[daily], func(t *testing.T) {
			c := configFor(t)
			if daily {
				c.DailyCapBytes = 100
			} else {
				c.TotalBudgetBytes = 100
				c.DailyCapBytes = 100
			}
			m := openFor(t, c)
			tk := admitFor(t, m)
			conn, _ := tk.WrapConn(&memoryConn{})
			_, _ = conn.Write(make([]byte, 100))
			_ = tk.Close()
			want := ErrBudget
			if daily {
				want = ErrDaily
			}
			if _, err := m.Admit(context.Background()); !errors.Is(err, want) {
				t.Fatal(err, want)
			}
		})
	}
}

func TestInvalidAndCorruptStateNeverResets(t *testing.T) {
	c := configFor(t)
	for _, mutate := range []func(*Config){func(c *Config) { c.StateDir = "relative" }, func(c *Config) { c.StateDir = "/" }, func(c *Config) { c.TotalBudgetBytes = 0 }, func(c *Config) { c.DailyCapBytes = -1 }, func(c *Config) { c.MaxRequestBytes = 0 }, func(c *Config) { c.MaxInflight = 0 }, func(c *Config) { c.InitialUsedBytes = -1 }, func(c *Config) { c.FlushInterval = -time.Second }} {
		bad := c
		mutate(&bad)
		if _, err := Open(bad); !errors.Is(err, ErrConfig) {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"not-json", "{}", `{"version":1,"day":"2026-11-17","total_out_bytes":-1}`} {
		p := filepath.Join(c.StateDir, "ledger.json")
		_ = os.WriteFile(p, []byte(bad), 0600)
		if _, err := Open(c); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(p)
		if string(after) != bad {
			t.Fatal("corrupt state overwritten")
		}
	}
}

func TestLockAndWriteFailureAreClosed(t *testing.T) {
	c := configFor(t)
	m := openFor(t, c)
	if _, err := Open(c); !errors.Is(err, ErrUnavailable) {
		t.Fatal("double owner accepted", err)
	}
	tk := admitFor(t, m)
	conn, _ := tk.WrapConn(&memoryConn{})
	_, _ = conn.Write([]byte("abc"))
	if err := os.Rename(c.StateDir, c.StateDir+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := tk.Close(); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := m.Admit(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := os.Rename(c.StateDir+"-moved", c.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := m.ledger.Flush(); err != nil {
		t.Fatal(err)
	}
	admitFor(t, m)
}

func TestCancellationAndNilInputs(t *testing.T) {
	m := openFor(t, configFor(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Admit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := m.Admit(nil); !errors.Is(err, ErrUnavailable) { //nolint:staticcheck // Deliberately exercise rejection of an invalid caller context.
		t.Fatal(err)
	}
	var nilM *Manager
	var nilT *Ticket
	if _, err := nilM.Admit(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if nilM.Close() != nil || nilT.Close() != nil || !nilM.Status().Closed || nilT.Usage().TotalBytes != 0 {
		t.Fatal("nil behavior")
	}
	raw := &memoryConn{}
	if _, err := nilT.WrapConn(raw); !errors.Is(err, ErrClosed) || !raw.closed.Load() {
		t.Fatal(err)
	}
	if _, err := nilT.WrapConn(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestUTCDateRolloverKeepsLifetimeUsage(t *testing.T) {
	c := configFor(t)
	now := time.Date(2026, 11, 16, 23, 59, 59, 0, time.UTC)
	l, err := openByteLedger(c.StateDir, 0, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.AddIn(10)
	l.AddOut(20)
	l.AddRequest()
	now = now.Add(2 * time.Second)
	l.AddIn(7)
	s := l.byteSnapshot()
	if s.Total != 37 || s.Day != 7 || s.DayKey != "2026-11-17" {
		t.Fatal(s)
	}
	if ok, _ := l.Exhausted(30, 100); !ok {
		t.Fatal("total cap missed")
	}
	if ok, _ := l.Exhausted(100, 5); !ok {
		t.Fatal("daily cap missed")
	}
	if ok, _ := l.Exhausted(100, 100); ok {
		t.Fatal("false exhausted")
	}
	if ok, _ := l.Reserve(0, 0, 0); !ok {
		t.Fatal("zero reservation failed")
	}
}

func TestCrashRecoveryChargesUnsettledAllowanceOnce(t *testing.T) {
	c := configFor(t)
	m := openFor(t, c)
	tk := admitFor(t, m)
	conn, _ := tk.WrapConn(&memoryConn{})
	_, _ = conn.Write(make([]byte, 17))
	if err := m.ledger.Flush(); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(filepath.Join(c.StateDir, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	recovered := c
	recovered.StateDir = t.TempDir()
	if err = os.WriteFile(filepath.Join(recovered.StateDir, "ledger.json"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	m2 := openFor(t, recovered)
	if s := m2.Status(); s.TotalUsedBytes != 100 || s.ReservedBytes != 0 {
		t.Fatal("crash refilled unspent-looking reservation", s)
	}
	if err = m2.Close(); err != nil {
		t.Fatal(err)
	}
	m3 := openFor(t, recovered)
	if s := m3.Status(); s.TotalUsedBytes != 100 {
		t.Fatal("reservation charged more than once", s)
	}
	if err = tk.Close(); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.TotalUsedBytes != 17 || s.ReservedBytes != 0 {
		t.Fatal("normal close did not settle actual bytes", s)
	}
}

func TestConcurrentCloseIsIdempotent(t *testing.T) {
	m := openFor(t, configFor(t))
	tk := admitFor(t, m)
	conn, _ := tk.WrapConn(&memoryConn{})
	_, _ = conn.Write([]byte("x"))
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = m.Close()
			} else {
				_ = tk.Close()
			}
		}(i)
	}
	wg.Wait()
	if s := m.Status(); s.Inflight != 0 || s.ReservedBytes != 0 || !s.Closed || s.TotalUsedBytes != 1 {
		t.Fatal(s)
	}
}

var _ net.Conn = (*memoryConn)(nil)

// Adapted from go-search-duck-go/internal/lane/ledger.go; existing lane state remains owned by its service.
package proxybudget

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// byteLedger is the lane's persistent byte account. Every byte read from or written to the upstream gateway is added with AddIn / AddOut (atomics, no
// lock on the hot path); a background loop writes the file about once a second when it changed, and Close writes it a last time. A crash loses at
// most one flush interval of counts (about a second of traffic): the gap between the 45 GB default budget and the 50 GB trial is the margin for that.
//
// A ledger file that exists but cannot be read is NOT reset to zero: Open fails and the lane stays closed, because zero would hand the budget back.
type byteLedger struct {
	path string
	lock *os.File
	now  func() time.Time

	totalIn, totalOut atomic.Int64
	dayIn, dayOut     atomic.Int64
	requests          atomic.Int64
	dayRequests       atomic.Int64
	reserved          atomic.Int64
	dirty             atomic.Bool
	failed            atomic.Bool
	flushMu           sync.Mutex

	mu     sync.Mutex // guards day and the file write
	day    string
	dayNum atomic.Int64 // days since the epoch of day, so the hot path compares one integer

	stop chan struct{}
	done chan struct{}
}

type ledgerFile struct {
	Version     int       `json:"version"`
	TotalIn     int64     `json:"total_in_bytes"`
	TotalOut    int64     `json:"total_out_bytes"`
	Requests    int64     `json:"requests"`
	Day         string    `json:"day"`
	DayIn       int64     `json:"day_in_bytes"`
	DayOut      int64     `json:"day_out_bytes"`
	DayRequests int64     `json:"day_requests"`
	Reserved    int64     `json:"reserved_bytes,omitempty"`
	Updated     time.Time `json:"updated"`
}

// ErrbyteLedgerLocked is returned by openByteLedger when another process holds the ledger (two lanes would double-spend the same budget).
var errLedgerLocked = errors.New("proxybudget: ledger is locked by another process")

// openByteLedger opens (creating if absent) dir/ledger.json under an exclusive lock and starts the flush loop. initialUsed seeds a new file only.
func openByteLedger(dir string, initialUsed int64, flush time.Duration, now func() time.Time) (*byteLedger, error) {
	if now == nil {
		now = time.Now
	}
	if flush <= 0 {
		flush = time.Second
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lane state dir: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "ledger.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lane ledger lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errLedgerLocked
	}
	l := &byteLedger{path: filepath.Join(dir, "ledger.json"), lock: lock, now: now, stop: make(chan struct{}), done: make(chan struct{})}
	var recoveredReservation int64
	b, err := os.ReadFile(l.path)
	switch {
	case err == nil:
		var f ledgerFile
		jerr := json.Unmarshal(b, &f)
		_, dateErr := time.Parse("2006-01-02", f.Day)
		if jerr != nil || f.Version != 1 || dateErr != nil || f.TotalIn < 0 || f.TotalOut < 0 ||
			f.TotalIn > math.MaxInt64-f.TotalOut || f.DayIn < 0 || f.DayOut < 0 ||
			f.DayIn > f.TotalIn || f.DayOut > f.TotalOut || f.Requests < 0 || f.DayRequests < 0 || f.DayRequests > f.Requests ||
			f.Reserved < 0 || f.Reserved > math.MaxInt64-f.TotalIn-f.TotalOut {
			lock.Close()
			return nil, fmt.Errorf("lane ledger %s is unreadable (not resetting it to zero): %v", l.path, jerr)
		}
		l.totalIn.Store(f.TotalIn)
		l.totalOut.Store(f.TotalOut)
		l.requests.Store(f.Requests)
		l.day = f.Day
		l.dayNum.Store(-1) // forces rollDay to compare the stored date with today's
		if t, perr := time.Parse("2006-01-02", f.Day); perr == nil {
			l.dayNum.Store(t.Unix() / 86400)
		}
		l.dayIn.Store(f.DayIn)
		l.dayOut.Store(f.DayOut)
		l.dayRequests.Store(f.DayRequests)
		recoveredReservation = f.Reserved
	case errors.Is(err, os.ErrNotExist):
		l.totalOut.Store(initialUsed) // bytes already spent elsewhere on the account; direction unknown, counted once
		l.dirty.Store(true)
	default:
		lock.Close()
		return nil, fmt.Errorf("lane ledger read: %w", err)
	}
	l.rollDay()
	// An unclosed request may have spent its entire held allowance before a
	// crash. Charge that allowance conservatively; never give it back on restart.
	if recoveredReservation > 0 {
		l.AddOut(recoveredReservation)
	}
	if err := l.Flush(); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lane ledger write: %w", err)
	}
	go l.loop(flush)
	return l, nil
}

func (l *byteLedger) loop(flush time.Duration) {
	defer close(l.done)
	t := time.NewTicker(flush)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = l.Flush()
		case <-l.stop:
			return
		}
	}
}

// rollDay starts a new UTC day's count when the date changed. The common case is one atomic load.
func (l *byteLedger) rollDay() {
	now := l.now().UTC()
	if now.Unix()/86400 == l.dayNum.Load() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDayLocked(now)
}

func (l *byteLedger) rollDayLocked(now time.Time) {
	today := now.UTC().Format("2006-01-02")
	l.dayNum.Store(now.Unix() / 86400)
	if l.day != today {
		l.day = today
		l.dayIn.Store(0)
		l.dayOut.Store(0)
		l.dayRequests.Store(0)
		l.dirty.Store(true)
	}
}

// AddIn and AddOut book bytes read from / written to the upstream gateway. They are called from the connection's Read and Write.
func (l *byteLedger) AddIn(n int64) {
	if n > 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.rollDayLocked(l.now())
		l.totalIn.Add(n)
		l.dayIn.Add(n)
		l.dirty.Store(true)
	}
}

func (l *byteLedger) AddOut(n int64) {
	if n > 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.rollDayLocked(l.now())
		l.totalOut.Add(n)
		l.dayOut.Add(n)
		l.dirty.Store(true)
	}
}

// AddRequest counts one lane request that was admitted.
func (l *byteLedger) AddRequest() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDayLocked(l.now())
	l.requests.Add(1)
	l.dayRequests.Add(1)
	l.dirty.Store(true)
}

// byteSnapshot is a consistent-enough read of the ledger.
type byteSnapshot struct {
	TotalIn, TotalOut, Total int64
	DayIn, DayOut, Day       int64
	Requests, DayRequests    int64
	Reserved                 int64
	DayKey                   string
}

func (l *byteLedger) byteSnapshot() byteSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDayLocked(l.now())
	s := byteSnapshot{
		TotalIn: l.totalIn.Load(), TotalOut: l.totalOut.Load(),
		DayIn: l.dayIn.Load(), DayOut: l.dayOut.Load(),
		Requests: l.requests.Load(), DayRequests: l.dayRequests.Load(),
		Reserved: l.reserved.Load(),
	}
	s.Total, s.Day = s.TotalIn+s.TotalOut, s.DayIn+s.DayOut
	s.DayKey = l.day
	return s
}

// Reserve holds n bytes against the limits for one request in flight when used + reserved + n still fits both, and reports false when it does not.
func (l *byteLedger) Reserve(n, total, daily int64) (ok bool, reason string) {
	if n <= 0 {
		return true, ""
	}
	// Serialize the usage+reservation check with other admissions. Socket I/O
	// never holds this lock; active tickets already carry their reservation.
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDayLocked(l.now())
	usedTotal := l.totalIn.Load() + l.totalOut.Load()
	usedDay := l.dayIn.Load() + l.dayOut.Load()
	r := l.reserved.Load() + n
	switch {
	case total > 0 && usedTotal+r > total:
		return false, "budget_exhausted"
	case daily > 0 && usedDay+r > daily:
		return false, "daily_cap"
	}
	l.reserved.Add(n)
	l.dirty.Store(true)
	return true, ""
}

// Unreserve gives back a Reserve.
func (l *byteLedger) Unreserve(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved.Add(-n)
	l.dirty.Store(true)
}

// settleBytes atomically moves held allowance into actual usage so a flush
// cannot observe spent bytes missing from both the usage and reservation.
func (l *byteLedger) settleBytes(n int64, read bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDayLocked(l.now())
	if read {
		l.totalIn.Add(n)
		l.dayIn.Add(n)
	} else {
		l.totalOut.Add(n)
		l.dayOut.Add(n)
	}
	l.reserved.Add(-n)
	l.dirty.Store(true)
}

// Exhausted reports whether the used bytes alone have reached a limit (the hard stop applied inside a running connection). It takes no lock.
func (l *byteLedger) Exhausted(total, daily int64) (bool, string) {
	l.rollDay()
	if total > 0 && l.totalIn.Load()+l.totalOut.Load() >= total {
		return true, "budget_exhausted"
	}
	if daily > 0 && l.dayIn.Load()+l.dayOut.Load() >= daily {
		return true, "daily_cap"
	}
	return false, ""
}

// Flush writes the file when something changed (atomic: temp file, fsync, rename).
func (l *byteLedger) Flush() error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	if !l.dirty.Swap(false) {
		return nil
	}
	s := l.byteSnapshot()
	f := ledgerFile{Version: 1, TotalIn: s.TotalIn, TotalOut: s.TotalOut, Requests: s.Requests, Day: s.DayKey,
		DayIn: s.DayIn, DayOut: s.DayOut, DayRequests: s.DayRequests, Reserved: s.Reserved, Updated: l.now().UTC()}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tmp := l.path + ".tmp"
	w, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		l.dirty.Store(true)
		l.failed.Store(true)
		return err
	}
	if _, err = w.Write(b); err == nil {
		err = w.Sync()
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, l.path)
	}
	if err == nil {
		var dir *os.File
		dir, err = os.Open(filepath.Dir(l.path))
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	if err != nil {
		l.dirty.Store(true)
	}
	l.failed.Store(err != nil)
	return err
}

// Close stops the flush loop, writes the file a last time and releases the lock.
func (l *byteLedger) Close() error {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
	l.dirty.Store(true)
	err := l.Flush()
	if l.lock != nil {
		_ = syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
		l.lock.Close()
	}
	return err
}

package egressproxy

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// watchdog fires fire once nothing called touch for d. touch is cheap enough
// to call on every read and write.
type watchdog struct {
	t *time.Timer
	d time.Duration
}

func newWatchdog(d time.Duration, fire func()) *watchdog {
	return &watchdog{t: time.AfterFunc(d, fire), d: d}
}

func (w *watchdog) touch() { w.t.Reset(w.d) }
func (w *watchdog) stop()  { w.t.Stop() }

// activityConn reports every byte it moves to the session watchdog.
type activityConn struct {
	net.Conn
	wd *watchdog
}

func (a *activityConn) Read(p []byte) (int, error) {
	n, err := a.Conn.Read(p)
	if n > 0 {
		a.wd.touch()
	}
	return n, err
}

func (a *activityConn) Write(p []byte) (int, error) {
	n, err := a.Conn.Write(p)
	if n > 0 {
		a.wd.touch()
	}
	return n, err
}

// CloseWrite half-closes the connection when the underlying type supports it.
func (a *activityConn) CloseWrite() error {
	if cw, ok := a.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// errByteCap is returned by cappedReader when MaxTunnelBytes is exceeded.
var errByteCap = errors.New("egressproxy: byte cap exceeded")

// splice relays between the client and the upstream until one side ends, the
// idle watchdog fires or a direction exceeds MaxTunnelBytes. Bytes the readers
// already buffered (br for the client, upBr for the upstream) are relayed
// first. It returns true when it ended because of the byte cap.
func (s *session) splice(upstream net.Conn, clientSrc, upSrc io.Reader) (capped bool) {
	p := s.p
	p.m.active.Inc()
	defer p.m.active.Dec()

	var wg sync.WaitGroup
	var cappedFlag atomic.Bool
	pipe := func(dst net.Conn, src io.Reader, counter prometheus.Counter) {
		defer wg.Done()
		hitCap, err := copyCapped(dst, src, p.cfg.MaxTunnelBytes, counter)
		if hitCap {
			cappedFlag.Store(true)
		}
		if hitCap || err != nil {
			// The cap, or a reset or write failure: the pair is no use any more.
			s.closeAll()
			return
		}
		// One side finished: pass the FIN on and let the other direction drain.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	wg.Add(2)
	go pipe(upstream, clientSrc, p.m.bytesUp)
	go pipe(s.client, upSrc, p.m.bytesDown)
	wg.Wait()
	return cappedFlag.Load()
}

// copyCapped copies src to dst, adding to counter as it goes. limit <= 0 means
// no cap. It reports whether the cap was hit and the first read or write error
// (a clean EOF is not an error).
func copyCapped(dst io.Writer, src io.Reader, limit int64, counter prometheus.Counter) (hitCap bool, err error) {
	bp := copyBufs.Get().(*[]byte)
	defer copyBufs.Put(bp)
	buf := *bp
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if limit > 0 && total+int64(n) > limit {
				chunk = chunk[:limit-total]
				hitCap = true
			}
			if len(chunk) > 0 {
				w, werr := dst.Write(chunk)
				total += int64(w)
				counter.Add(float64(w))
				if werr != nil {
					return false, werr
				}
			}
			if hitCap {
				return true, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return false, rerr
		}
	}
}

// cappedBody wraps a request or response body so a plain HTTP exchange obeys
// the same byte cap as a tunnel and is counted in bytes_total.
type cappedBody struct {
	rc      io.ReadCloser
	limit   int64
	n       int64
	counter prometheus.Counter
	capped  *atomic.Bool
	// beforeRead, when set, runs before each read from rc (used to flush a
	// write buffer before blocking on the upstream).
	beforeRead func()
	// readErr is the first error rc (or the cap) returned other than EOF. For a
	// request body it tells a failing browser from a failing upstream.
	readErr error
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		b.beforeRead()
	}
	if b.limit > 0 {
		remaining := b.limit - b.n
		if remaining <= 0 {
			// One more byte would exceed the cap: probe for EOF first.
			var one [1]byte
			n, err := b.rc.Read(one[:])
			if n > 0 {
				b.capped.Store(true)
				b.readErr = errByteCap
				return 0, errByteCap
			}
			if err != nil && err != io.EOF {
				b.readErr = err
			}
			return 0, err
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := b.rc.Read(p)
	b.n += int64(n)
	b.counter.Add(float64(n))
	if err != nil && err != io.EOF && b.readErr == nil {
		b.readErr = err
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }

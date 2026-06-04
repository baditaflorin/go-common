package reqstats

import (
	"net/http"
	"strconv"
)

// Option configures the middleware.
type Option func(*mwConfig)

type mwConfig struct{ heapDelta bool }

// WithHeapDelta also records a per-request heap-alloc delta (adds a
// runtime.ReadMemStats stop-the-world per request). Opt-in: use on services
// that want a RAM hint and can afford it; leave off for high-QPS services.
func WithHeapDelta() Option { return func(c *mwConfig) { c.heapDelta = true } }

// Middleware returns a go-common middleware that emits per-request Server-Timing
// + X-Request-Stats on every response. svc/ver label the stats. The universal
// payload (total + bytes + approx CPU) needs no per-handler code; handlers
// enrich via reqstats.From(r.Context()).
func Middleware(svc, ver string, opts ...Option) func(http.Handler) http.Handler {
	var c mwConfig
	for _, o := range opts {
		o(&c)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := Start(svc, ver).EnableApprox()
			if c.heapDelta {
				t.EnableHeapDelta()
			}
			if r.ContentLength > 0 {
				t.AddBytesIn(r.ContentLength)
			}
			sw := &statsWriter{ResponseWriter: w, t: t}
			next.ServeHTTP(sw, r.WithContext(NewContext(r.Context(), t)))
			sw.ensure() // handlers that never write still get the headers + finalize
		})
	}
}

// statsWriter injects the stats headers when the response begins (first
// WriteHeader/Write), so they reflect everything up to the first byte — which
// is where the work happens (render, then body). bytes.out is taken from an
// explicit Content-Length if the handler set one.
type statsWriter struct {
	http.ResponseWriter
	t     *Tracker
	wrote bool
}

func (sw *statsWriter) WriteHeader(code int) {
	if sw.wrote {
		return
	}
	sw.wrote = true
	if cl := sw.Header().Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			sw.t.SetBytesOut(n)
		}
	}
	sw.t.writeHeaders(sw.Header(), code < 500)
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statsWriter) Write(b []byte) (int, error) {
	if !sw.wrote {
		sw.WriteHeader(http.StatusOK)
	}
	return sw.ResponseWriter.Write(b)
}

// Flush preserves streaming handlers (SSE); finalizes headers first.
func (sw *statsWriter) Flush() {
	if !sw.wrote {
		sw.WriteHeader(http.StatusOK)
	}
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statsWriter) ensure() {
	if !sw.wrote {
		sw.WriteHeader(http.StatusOK)
	}
}

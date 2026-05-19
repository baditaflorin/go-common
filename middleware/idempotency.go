package middleware

import (
	"bytes"
	"container/list"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// IdempotencyKeyHeader is the request header used to key the cache.
// Standard across the fleet — clients retrying a mutating request set
// the same key so the server can dedupe.
const IdempotencyKeyHeader = "Idempotency-Key"

// IdempotencyCachedHeader is set on replayed responses so a client (or
// a debug proxy) can tell a cached reply from a freshly-computed one.
const IdempotencyCachedHeader = "Idempotency-Cached"

// IdempotencyConfig configures the idempotency-key cache.
type IdempotencyConfig struct {
	// TTL is the maximum age of a cached response. After TTL elapses,
	// the next request with the same key re-executes the handler.
	// Default: 1 hour.
	TTL time.Duration

	// MaxEntries caps the in-memory cache size to bound RAM. Eviction is
	// LRU. Default: 10000.
	MaxEntries int

	// RequiredMethods is the set of HTTP methods that consult the cache.
	// Default: {POST, PUT, PATCH, DELETE}. GET/HEAD are skipped (no
	// body, idempotent by HTTP spec).
	RequiredMethods []string

	// now is an injectable clock for tests. nil = time.Now.
	now func() time.Time
}

func (c IdempotencyConfig) withDefaults() IdempotencyConfig {
	if c.TTL <= 0 {
		c.TTL = time.Hour
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = 10000
	}
	if len(c.RequiredMethods) == 0 {
		c.RequiredMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// cachedResponse is the persisted server reply for a given key.
type cachedResponse struct {
	status      int
	contentType string
	body        []byte
	expiresAt   time.Time
}

// idempotencyCache is a bounded LRU + TTL cache plus a singleflight
// group to dedupe concurrent requests sharing the same key.
type idempotencyCache struct {
	cfg     IdempotencyConfig
	methods map[string]struct{}
	sf      singleflight.Group

	mu      sync.Mutex
	entries map[string]*list.Element // key -> list element
	lru     *list.List               // front = most recent
}

type lruItem struct {
	key   string
	value cachedResponse
}

func newIdempotencyCache(cfg IdempotencyConfig) *idempotencyCache {
	cfg = cfg.withDefaults()
	methods := make(map[string]struct{}, len(cfg.RequiredMethods))
	for _, m := range cfg.RequiredMethods {
		methods[m] = struct{}{}
	}
	return &idempotencyCache{
		cfg:     cfg,
		methods: methods,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
	}
}

// get returns the cached response for key if present and unexpired.
func (c *idempotencyCache) get(key string) (cachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return cachedResponse{}, false
	}
	item := el.Value.(*lruItem)
	if !item.value.expiresAt.After(c.cfg.now()) {
		// Expired — drop it.
		c.lru.Remove(el)
		delete(c.entries, key)
		return cachedResponse{}, false
	}
	c.lru.MoveToFront(el)
	return item.value, true
}

// put stores a response under key, applying LRU eviction.
func (c *idempotencyCache) put(key string, v cachedResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*lruItem).value = v
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&lruItem{key: key, value: v})
	c.entries[key] = el
	for c.lru.Len() > c.cfg.MaxEntries {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*lruItem).key)
	}
}

// captureWriter buffers the handler's response in memory; bytes are
// not forwarded to any real client. The idempotency middleware
// replays the captured response on every caller (leader + followers).
type captureWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: http.Header{}}
}

func (rw *captureWriter) Header() http.Header { return rw.header }

func (rw *captureWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.status = code
	rw.wroteHeader = true
}

func (rw *captureWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.body.Write(b)
}

// IdempotencyKey returns a middleware that dedupes mutating requests
// bearing an Idempotency-Key header. See IdempotencyConfig for tuning.
//
// Behavior:
//   - Empty header: passthrough (no caching).
//   - Method not in RequiredMethods (default POST/PUT/PATCH/DELETE):
//     passthrough.
//   - Cache HIT: cached body, status, and Content-Type are replayed.
//     The handler is NOT invoked. Idempotency-Cached: true is set.
//   - Cache MISS: handler runs once (via singleflight). 2xx and 4xx
//     responses are cached; 5xx is NOT (transient errors should be
//     retriable, not poisoned into the cache).
//
// Caveats:
//   - Only body, status, and Content-Type are cached. Custom headers
//     set by the handler do not survive a replay.
//   - The cache is process-local. Multi-replica services need an
//     external store (out of scope for this middleware).
func IdempotencyKey(cfg IdempotencyConfig) Middleware {
	cache := newIdempotencyCache(cfg)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(IdempotencyKeyHeader)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := cache.methods[r.Method]; !ok {
				next.ServeHTTP(w, r)
				return
			}

			// Fast path: cached + unexpired → replay without invoking
			// the handler at all.
			if v, ok := cache.get(key); ok {
				replay(w, v, true)
				return
			}

			// Slow path: singleflight ensures concurrent requests with
			// the same key result in exactly one handler invocation.
			// The leader executes the handler against its OWN writer
			// (streaming to its own client) and stores the response in
			// the cache. Followers receive the leader's return value
			// (shared=true) and replay from cache against their own
			// writer.
			//
			// To keep the leader's writer separate from followers, we
			// can't pass `w` into the singleflight closure (the
			// closure runs once but is observed by N callers). The
			// leader needs a separate recordingWriter that ALSO
			// streams to its real client. We handle that by having
			// the singleflight Do produce only the cached value;
			// every caller (leader and followers) then writes to its
			// own w from that cached value.
			//
			// Trade-off: the leader's response is buffered until the
			// handler finishes, then flushed in one go. This matches
			// typical JSON-API handlers but breaks streaming.

			result, _, shared := cache.sf.Do(key, func() (interface{}, error) {
				// Re-check the cache inside the singleflight in case
				// a sibling request just populated it.
				if v, ok := cache.get(key); ok {
					return &v, nil
				}

				// Capture-only writer — discard streamed writes
				// (every caller will write from the captured bytes).
				rec := newCaptureWriter()
				next.ServeHTTP(rec, r)
				if !rec.wroteHeader {
					rec.status = http.StatusOK
				}

				// Only cache 2xx and 4xx. 5xx is treated as a
				// transient failure that should be retried, not
				// memoized. We still want to propagate the response
				// to the leader's client, so return a non-cached
				// envelope.
				stored := cachedResponse{
					status:      rec.status,
					contentType: rec.header.Get("Content-Type"),
					body:        rec.body.Bytes(),
					expiresAt:   cache.cfg.now().Add(cache.cfg.TTL),
				}
				if rec.status < 500 {
					cache.put(key, stored)
				}
				return &stored, nil
			})

			if result == nil {
				return
			}
			v := result.(*cachedResponse)
			// If singleflight shared the result with other callers,
			// this caller is consuming a deduped response — mark it
			// cached for client visibility. If it's a solo leader,
			// the response is fresh.
			replay(w, *v, shared)
		})
	}
}

// replay writes a captured response back to the client. If cached is
// true (the response came from the cache OR was shared by the
// singleflight from another caller's handler invocation), an
// Idempotency-Cached: true header is set.
func replay(w http.ResponseWriter, v cachedResponse, cached bool) {
	if v.contentType != "" {
		w.Header().Set("Content-Type", v.contentType)
	}
	if cached {
		w.Header().Set(IdempotencyCachedHeader, "true")
	}
	status := v.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(v.body)
}

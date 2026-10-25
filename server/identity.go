package server

import (
	"net/http"
	"strings"

	"github.com/baditaflorin/go-common/header"
)

// maxIdentityValueLen bounds a name or version sent in a response header. A real
// one is a few dozen bytes; anything longer is a configuration mistake, and it is
// left out rather than sent on every response.
const maxIdentityValueLen = 256

// identityMiddleware puts header.ServiceName and header.ServiceVersion on every
// response next sends: success and error answers, the keystore gate's 401 and
// 403, the mux's 404, 405 and redirects, /health, /version, /metrics and the
// default /selftest. Server.Handler installs it outermost, so nothing it wraps
// can answer without them. A caller (DomainScope's enrichment client, for one)
// records the release that produced an answer from them when the body names
// none.
//
// The server's values win. They are set before next runs, so a handler that
// never writes still sends them, and set again when the response starts (the
// first WriteHeader, Write or Flush), so a value a handler set, added or deleted
// is replaced. The headers always name the process that wrote the response: a
// service that relays another answer (the fleet fetch cache replays the headers
// it stored from the origin) cannot pass on someone else's name or release, and
// neither can a website whose headers it copies. A relay that wants to say who
// produced the body says it in a header of its own.
//
// A value that is empty, longer than maxIdentityValueLen or holds a control
// character is not sent; with neither value usable, next is returned unwrapped.
func identityMiddleware(name, version string, next http.Handler) http.Handler {
	id := serviceIdentity{name: identityValue(name), version: identityValue(version)}
	if id.name == "" && id.version == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iw := &identityWriter{ResponseWriter: w, id: id}
		iw.preset()
		next.ServeHTTP(iw, r)
		if !iw.started {
			// The handler returned without writing: net/http sends the header map as
			// it is now, so undo whatever the handler did to the two headers.
			id.apply(w.Header())
		}
	})
}

// identityValue is s as a response header value, or "" when s cannot be one.
func identityValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxIdentityValueLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return ""
		}
	}
	return s
}

// serviceIdentity is the name and release a Server states on every response.
type serviceIdentity struct {
	name    string
	version string
}

// apply makes the two headers hold exactly the server's values. It allocates only
// for a header that does not already hold exactly that value.
func (id serviceIdentity) apply(h http.Header) {
	if id.name != "" {
		setExactly(h, header.ServiceName, id.name)
	}
	if id.version != "" {
		setExactly(h, header.ServiceVersion, id.version)
	}
}

// setExactly sets key (which must be in canonical form) to the single value v,
// unless it already holds exactly that.
func setExactly(h http.Header, key, v string) {
	if vs := h[key]; len(vs) == 1 && vs[0] == v {
		return
	}
	h[key] = []string{v}
}

// identityWriter applies the server's identity when the response starts.
//
// It forwards Flush and exposes the wrapped writer through Unwrap, as the
// middleware writers do, so streaming handlers and http.ResponseController
// (Hijack, deadlines) reach the connection.
type identityWriter struct {
	http.ResponseWriter
	id serviceIdentity
	// started is set once the final response header has gone out (a WriteHeader
	// of 200 or above, a Write or a Flush); 1xx informational headers do not count.
	started bool
	// vals backs the preset header values, so the common response costs one
	// allocation (this writer) and not one per header.
	vals [2]string
}

// preset puts the server's values on the header map before the handler runs.
func (iw *identityWriter) preset() {
	h := iw.ResponseWriter.Header()
	if iw.id.name != "" {
		iw.vals[0] = iw.id.name
		h[header.ServiceName] = iw.vals[0:1:1]
	}
	if iw.id.version != "" {
		iw.vals[1] = iw.id.version
		h[header.ServiceVersion] = iw.vals[1:2:2]
	}
}

func (iw *identityWriter) begin(code int) {
	if iw.started {
		return
	}
	iw.id.apply(iw.ResponseWriter.Header())
	if code < 100 || code > 199 {
		iw.started = true
	}
}

func (iw *identityWriter) WriteHeader(code int) {
	iw.begin(code)
	iw.ResponseWriter.WriteHeader(code)
}

func (iw *identityWriter) Write(b []byte) (int, error) {
	iw.begin(http.StatusOK)
	return iw.ResponseWriter.Write(b)
}

// Flush sends the header (with the identity) and any buffered body.
func (iw *identityWriter) Flush() { _ = iw.FlushError() }

// FlushError is Flush with the error http.ResponseController reports. A flush
// the wrapped writer cannot do has not started the response.
func (iw *identityWriter) FlushError() error {
	if !iw.started {
		iw.id.apply(iw.ResponseWriter.Header())
	}
	err := http.NewResponseController(iw.ResponseWriter).Flush()
	if err == nil {
		iw.started = true
	}
	return err
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (iw *identityWriter) Unwrap() http.ResponseWriter { return iw.ResponseWriter }

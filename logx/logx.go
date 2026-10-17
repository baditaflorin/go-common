package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Redacted replaces a redacted value.
const Redacted = "[redacted]"

// Options configures a logger.
type Options struct {
	// Service, Version and Node are added to every record when non-empty.
	Service string
	Version string
	Node    string
	// Level is the minimum level; the default is slog.LevelInfo.
	Level slog.Leveler
	// Output defaults to os.Stdout.
	Output io.Writer
	// AddSource adds the file and line of the call.
	AddSource bool
	// ContextAttrs adds attributes derived from each record's context. Do not
	// return one that the logger already carries (the request logger has
	// request_id): the JSON would then hold the key twice.
	ContextAttrs func(ctx context.Context) []slog.Attr
}

type handler struct {
	inner    slog.Handler
	ctxAttrs func(context.Context) []slog.Attr
}

// NewHandler returns the slog.Handler.
func NewHandler(o Options) slog.Handler {
	out := o.Output
	if out == nil {
		out = os.Stdout
	}
	var inner slog.Handler = slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:       o.Level,
		AddSource:   o.AddSource,
		ReplaceAttr: redactAttr,
	})
	var fixed []slog.Attr
	for _, kv := range [][2]string{{"service", o.Service}, {"version", o.Version}, {"node", o.Node}} {
		if kv[1] != "" {
			fixed = append(fixed, slog.String(kv[0], kv[1]))
		}
	}
	if len(fixed) > 0 {
		inner = inner.WithAttrs(fixed)
	}
	return &handler{inner: inner, ctxAttrs: o.ContextAttrs}
}

// New returns a logger using NewHandler.
func New(o Options) *slog.Logger { return slog.New(NewHandler(o)) }

// Install makes the logger the process default for slog and, because slog.SetDefault
// does so, for the standard library's log package too. It returns the logger.
func Install(o Options) *slog.Logger {
	l := New(o)
	slog.SetDefault(l)
	return l
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	if h.ctxAttrs != nil {
		r.AddAttrs(h.ctxAttrs(ctx)...)
	}
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{inner: h.inner.WithAttrs(attrs), ctxAttrs: h.ctxAttrs}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name), ctxAttrs: h.ctxAttrs}
}

var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|passw(or)?d|passwd|authorization|api[_-]?key|apikey|cookie|credential|private[_-]?key)`)

var (
	bearerRe   = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	userinfoRe = regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]*@`)
	paramRe    = regexp.MustCompile(`(?i)\b((?:api[_-]?key|token|secret|passw(?:or)?d|access_token)=)[^&\s"]+`)
)

// RedactString removes the credentials a string carries: bearer tokens, the
// user and password of a URL, and token=, password=, secret= or api_key= values.
func RedactString(s string) string {
	if !mayCarrySecret(s) {
		return s
	}
	return redactAll(s)
}

// redactAll applies every pattern unconditionally. RedactString calls it only when
// the cheap pre-check says it might matter; FuzzRedactString checks that skipping it
// never changes the result.
func redactAll(s string) string {
	s = bearerRe.ReplaceAllString(s, "$1 "+Redacted)
	s = userinfoRe.ReplaceAllString(s, "${1}"+Redacted+"@")
	return paramRe.ReplaceAllString(s, "${1}"+Redacted)
}

// mayCarrySecret is a cheap test for what the patterns above need: an '@' for a
// URL's credentials, the word "bearer", or an '=' directly after a name that ends
// in key, token, secret, password or passwd. Almost every log string has none of
// these (even "status=200" does not), and skipping the regular expressions for them
// keeps redaction close to free. It must stay a superset of what the patterns can
// match, or a secret would slip through unchecked.
func mayCarrySecret(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "@") {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case (c == 'b' || c == 'B') && i+6 <= len(s) && strings.EqualFold(s[i:i+6], "bearer"):
			return true
		case c == '=' && endsWithParamName(s[:i]):
			return true
		}
	}
	return false
}

// paramNameSuffixes are the endings of the names paramRe accepts: api_key, api-key,
// apikey, access_token, token, secret, password and passwd.
var paramNameSuffixes = []string{"key", "token", "secret", "password", "passwd"}

func endsWithParamName(s string) bool {
	for _, n := range paramNameSuffixes {
		if len(s) >= len(n) && strings.EqualFold(s[len(s)-len(n):], n) {
			return true
		}
	}
	return false
}

// redactAttr is the slog ReplaceAttr: it redacts string values, all of them, and
// any string stored under a credential-looking key.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		a.Value = a.Value.Resolve()
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if sensitiveKey.MatchString(a.Key) && a.Value.String() != "" {
			return slog.String(a.Key, Redacted)
		}
		a.Value = slog.StringValue(RedactString(a.Value.String()))
	case slog.KindAny:
		// An error or Stringer is logged as text; scrub that text too.
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(RedactString(err.Error()))
		}
	}
	return a
}

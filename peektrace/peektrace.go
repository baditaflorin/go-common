// Package peektrace carries a short-lived, operator-created domain-inspection
// session through internal fleet requests. It is deliberately opt-in: normal
// requests have no context value, no extra headers, and no cache-key impact.
package peektrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const (
	IDHeader     = "X-Fleet-Peek-ID"
	DomainHeader = "X-Fleet-Peek-Domain"
)

type Trace struct {
	ID     string
	Domain string
}

type contextKey struct{}

// NewID returns an unguessable 128-bit session identifier.
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NormalizeDomain accepts either a hostname or a URL and returns a lowercase
// hostname without a trailing dot. Ports, paths, credentials, and fragments
// are rejected rather than silently broadening the trace scope.
func NormalizeDomain(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(s, "@?#") {
		return "", errors.New("domain contains URL-only characters")
	}
	parse := s
	if !strings.Contains(s, "://") {
		parse = "https://" + s
	}
	u, err := url.Parse(parse)
	if err != nil || u.Hostname() == "" || u.Port() != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("invalid domain")
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(u.Hostname())), ".")
	if host == "" || strings.ContainsAny(host, " /\\") {
		return "", errors.New("invalid domain hostname")
	}
	return host, nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// FromHeaders validates and extracts a trace. Invalid or incomplete headers
// are ignored, which keeps externally supplied garbage from affecting normal
// fleet calls.
func FromHeaders(h http.Header) (Trace, bool) {
	id := strings.TrimSpace(h.Get(IDHeader))
	domain, err := NormalizeDomain(h.Get(DomainHeader))
	if !validID(id) || err != nil {
		return Trace{}, false
	}
	return Trace{ID: id, Domain: domain}, true
}

func WithTrace(ctx context.Context, t Trace) context.Context {
	if !validID(t.ID) {
		return ctx
	}
	domain, err := NormalizeDomain(t.Domain)
	if err != nil {
		return ctx
	}
	t.Domain = domain
	return context.WithValue(ctx, contextKey{}, t)
}

func FromContext(ctx context.Context) (Trace, bool) {
	t, ok := ctx.Value(contextKey{}).(Trace)
	return t, ok && validID(t.ID) && t.Domain != ""
}

func ApplyHeaders(ctx context.Context, h http.Header) {
	t, ok := FromContext(ctx)
	if !ok {
		return
	}
	h.Set(IDHeader, t.ID)
	h.Set(DomainHeader, t.Domain)
}

// Middleware imports only validated internal headers into context. It never
// writes them to responses and does not change cache identity.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := FromHeaders(r.Header); ok {
			r = r.WithContext(WithTrace(r.Context(), t))
		}
		next.ServeHTTP(w, r)
	})
}

// HostInScope includes the apex and its conventional www redirect target.
func HostInScope(domain, target string) bool {
	want, err := NormalizeDomain(domain)
	if err != nil {
		return false
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return false
		}
		target = u.Hostname()
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target)), ".")
	return host == want || host == "www."+want || host == strings.TrimPrefix(want, "www.")
}

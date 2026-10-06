// Package spiffe adapts the SPIFFE Workload API to credentiallease identities.
package spiffe

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

var ErrUnavailable = errors.New("credentiallease/spiffe: workload identity unavailable")

type jwtSource interface {
	FetchJWTSVID(context.Context, jwtsvid.Params) (*jwtsvid.SVID, error)
	Close() error
}

// Source obtains short-lived JWT-SVIDs for one requested audience. It must be
// closed when the owning service shuts down.
type Source struct {
	source jwtSource
}

// New creates a Workload API JWT source. socket must be a validated SPIFFE
// Workload API address, normally unix:///run/spire/agent.sock.
func New(ctx context.Context, socket string) (*Source, error) {
	socket = strings.TrimSpace(socket)
	if ctx == nil || !validUnixSocket(socket) {
		return nil, ErrUnavailable
	}
	source, err := workloadapi.NewJWTSource(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		return nil, ErrUnavailable
	}
	return &Source{source: source}, nil
}

func validUnixSocket(socket string) bool {
	u, err := url.Parse(socket)
	return err == nil && u.Scheme == "unix" && u.Path != "" && strings.HasPrefix(u.Path, "/") && u.Host == "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && workloadapi.ValidateAddress(socket) == nil
}

// Token returns a compact JWT-SVID minted for exactly audience. No token or
// Workload API error details are included in errors.
func (s *Source) Token(ctx context.Context, audience string) (string, error) {
	if s == nil || s.source == nil || ctx == nil || audience == "" || len(audience) > 512 || strings.TrimSpace(audience) != audience {
		return "", ErrUnavailable
	}
	svid, err := s.source.FetchJWTSVID(ctx, jwtsvid.Params{Audience: audience})
	if err != nil || svid == nil || len(svid.Audience) != 1 || svid.Audience[0] != audience || !svid.Expiry.After(time.Now()) {
		return "", ErrUnavailable
	}
	token := svid.Marshal()
	if token == "" || len(token) > 8192 || strings.ContainsAny(token, " \t\r\n") {
		return "", ErrUnavailable
	}
	return token, nil
}

func (s *Source) Close() error {
	if s == nil || s.source == nil {
		return nil
	}
	return s.source.Close()
}

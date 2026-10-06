package spiffe

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
)

type fakeJWTSource struct {
	svid *jwtsvid.SVID
	err  error
}

func (s fakeJWTSource) FetchJWTSVID(context.Context, jwtsvid.Params) (*jwtsvid.SVID, error) {
	return s.svid, s.err
}

func (fakeJWTSource) Close() error { return nil }

func TestSourceReturnsSingleAudienceUnexpiredSVID(t *testing.T) {
	token := testToken("broker", time.Now().Add(time.Minute))
	svid, err := jwtsvid.ParseInsecure(token, []string{"broker"})
	if err != nil {
		t.Fatal(err)
	}
	source := &Source{source: fakeJWTSource{svid: svid}}
	got, err := source.Token(context.Background(), "broker")
	if err != nil || got != token {
		t.Fatalf("expected matching SVID, got token set=%t err=%v", got != "", err)
	}
}

func TestSourceRejectsInvalidAudienceAndExpiredSVID(t *testing.T) {
	t.Run("wrong audience", func(t *testing.T) {
		svid, err := jwtsvid.ParseInsecure(testToken("other", time.Now().Add(time.Minute)), []string{"broker", "other"})
		if err != nil {
			t.Fatal(err)
		}
		source := &Source{source: fakeJWTSource{svid: svid}}
		if got, err := source.Token(context.Background(), "broker"); err == nil || got != "" {
			t.Fatal("wrong-audience SVID must fail closed")
		}
	})
	t.Run("expired token", func(t *testing.T) {
		if _, err := jwtsvid.ParseInsecure(testToken("broker", time.Now().Add(-time.Minute)), []string{"broker"}); err == nil {
			t.Fatal("expired SVID must be rejected by the maintained SPIFFE parser")
		}
	})
}

func TestSourceRejectsInvalidConfigurationAndUnavailableAPI(t *testing.T) {
	if _, err := New(context.Background(), "http://127.0.0.1:1234"); err == nil {
		t.Fatal("unexpected Workload API transport accepted")
	}
	if got, err := (*Source)(nil).Token(context.Background(), "broker"); err == nil || got != "" {
		t.Fatal("nil source must fail closed")
	}
}

func testToken(audience string, expiry time.Time) string {
	encode := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	header := encode(`{"alg":"ES256","typ":"JWT"}`)
	payload := encode(fmt.Sprintf(`{"sub":"spiffe://example.org/workload","aud":[%q],"exp":%d,"iat":%d}`, audience, expiry.Unix(), time.Now().Add(-time.Second).Unix()))
	return strings.Join([]string{header, payload, encode("signature")}, ".")
}

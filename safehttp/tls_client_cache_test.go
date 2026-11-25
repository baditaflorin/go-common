package safehttp

import (
	"crypto/tls"
	"testing"
)

func TestPrimaryTLSClientSessionCacheIsOptIn(t *testing.T) {
	o := &options{}
	if got := newBaseTransport(o, nil).TLSClientConfig; got != nil {
		t.Fatalf("default TLS configuration changed: %+v", got)
	}
	cache := tls.NewLRUClientSessionCache(1)
	WithTLSClientSessionCache(cache)(o)
	config := newBaseTransport(o, nil).TLSClientConfig
	if config == nil || config.ClientSessionCache != cache {
		t.Fatal("requested session cache is missing")
	}
	if config.InsecureSkipVerify || config.ServerName != "" || config.RootCAs != nil {
		t.Fatal("session cache changed the default certificate or hostname verification")
	}
}

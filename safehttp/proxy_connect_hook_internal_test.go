package safehttp

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

func TestWithProxyConnectResponseHookConfiguresTransport(t *testing.T) {
	called := false
	hook := func(context.Context, *url.URL, *http.Request, *http.Response) error {
		called = true
		return nil
	}
	o := &options{}
	WithProxyConnectResponseHook(hook)(o)
	transport := newBaseTransport(o, nil)
	if transport.OnProxyConnectResponse == nil {
		t.Fatal("proxy CONNECT hook was not configured on transport")
	}
	if err := transport.OnProxyConnectResponse(context.Background(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("proxy CONNECT hook was not invoked")
	}
}

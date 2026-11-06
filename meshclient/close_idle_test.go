package meshclient_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/meshclient"
)

func TestCloseIdleConnectionsClosesDefaultPoolAndClientRemainsUsable(t *testing.T) {
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	client, err := meshclient.New(meshclient.Config{Name: "test", BaseURL: server.URL, Mode: meshclient.FailClosed})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	var reply struct {
		OK bool `json:"ok"`
	}
	if err := client.Get(t.Context(), "/", &reply); err != nil || !reply.OK {
		t.Fatalf("first request: reply=%+v err=%v", reply, err)
	}
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("mesh idle pool was not closed")
	}
	reply.OK = false
	if err := client.Get(t.Context(), "/", &reply); err != nil || !reply.OK {
		t.Fatalf("request after cleanup: reply=%+v err=%v", reply, err)
	}
}

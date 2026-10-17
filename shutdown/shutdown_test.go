//go:build unix

package shutdown_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/shutdown"
	"github.com/baditaflorin/go-common/testhelpers/leakcheck"
)

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitUp(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never came up", addr)
}

func TestListenAndServeReturnsAStartupError(t *testing.T) {
	leakcheck.Check(t)
	srv := &http.Server{Addr: "127.0.0.1:-1"} // an invalid port: ListenAndServe fails at once
	if err := shutdown.ListenAndServe(srv); err == nil {
		t.Fatal("ListenAndServe on an invalid address returned nil")
	}
}

// The point of the package: on a signal, in-flight requests finish, new work is
// refused, and ListenAndServe returns nil.
func TestSignalDrainsInFlightRequests(t *testing.T) {
	leakcheck.Check(t)
	addr := freeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = io.WriteString(w, "finished")
		}),
	}
	notified := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() {
		result <- shutdown.ListenAndServe(srv,
			shutdown.WithSignals(syscall.SIGUSR1),
			shutdown.WithNotify(func(s os.Signal) { notified <- s }),
			shutdown.WithDrainTimeout(10*time.Second))
	}()
	waitUp(t, addr)

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("the Notify callback never ran")
	}

	select {
	case err := <-result:
		t.Fatalf("ListenAndServe returned (%v) while a request was still in flight", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	if got := <-body; got != "finished" {
		t.Fatalf("in-flight request got %q, want it to finish normally", got)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ListenAndServe after a clean drain = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after the drain")
	}
}

func TestDrainTimeoutIsReportedWhenARequestHangs(t *testing.T) {
	addr := freeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		}),
	}
	result := make(chan error, 1)
	go func() {
		result <- shutdown.ListenAndServe(srv,
			shutdown.WithSignals(syscall.SIGUSR1),
			shutdown.WithNotify(func(os.Signal) {}),
			shutdown.WithDrainTimeout(100*time.Millisecond))
	}()
	waitUp(t, addr)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get("http://" + addr + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ListenAndServe = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe ignored its drain timeout")
	}
	_ = srv.Close()
}

func TestDefaultsAreSane(t *testing.T) {
	if shutdown.DefaultDrainTimeout <= 0 || shutdown.DefaultDrainTimeout > time.Minute {
		t.Fatalf("DefaultDrainTimeout = %v, want a positive duration of at most a minute", shutdown.DefaultDrainTimeout)
	}
}

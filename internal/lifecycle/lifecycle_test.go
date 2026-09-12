package lifecycle

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeWaitsForAdmittedHandler(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	tracker := NewTracker()
	server := &http.Server{Handler: tracker.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusCreated)
	}))}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Serve(ctx, server, tracker, time.Second, func() error { return server.Serve(listener) })
		result <- err
	}()
	response := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		response <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		t.Fatalf("Serve returned while handler was active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-response; err != nil {
		t.Fatal(err)
	}
}

func TestServeTimeoutClosesConnectionThenDrainsHandler(t *testing.T) {
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	tracker := NewTracker()
	server := &http.Server{Handler: tracker.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		close(entered)
		_, _ = io.Copy(io.Discard, r.Body)
	}))}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		forced bool
		err    error
	}, 1)
	go func() {
		forced, err := Serve(ctx, server, tracker, 30*time.Millisecond, func() error { return server.Serve(listener) })
		result <- struct {
			forced bool
			err    error
		}{forced, err}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000000\r\n\r\n"+strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel()
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !got.forced {
		t.Fatal("timeout did not force-close active connection")
	}
	select {
	case <-handlerDone:
	default:
		t.Fatal("Serve returned before the interrupted handler exited")
	}
}

package gateway

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

type readSignal struct {
	io.ReadCloser
	once    sync.Once
	started chan struct{}
}

func (b *readSignal) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestWriterFailureInterruptsBlockedClientBody(t *testing.T) {
	testBlockedClientFinalResponse(t, "disconnect")
}

func TestSplitFinalResponseBodyIsPreserved(t *testing.T) {
	testBlockedClientFinalResponse(t, "split-response")
}

func testBlockedClientFinalResponse(t *testing.T, mode string) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "writer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	readStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			writerDone <- acceptErr
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, readErr := http.ReadRequest(bufio.NewReader(conn)); readErr != nil {
			writerDone <- readErr
			return
		}
		select {
		case <-readStarted:
		case <-time.After(time.Second):
			writerDone <- fmt.Errorf("receiver did not start reading the client body")
			return
		}
		if mode == "disconnect" {
			writerDone <- nil
			return
		}
		responseBody := "{\"error\":\"errore autorevole ritardato\"}\n"
		if _, writeErr := fmt.Fprintf(conn, "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(responseBody)); writeErr != nil {
			writerDone <- writeErr
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, writeErr := io.WriteString(conn, responseBody)
		writerDone <- writeErr
	}()

	gateway := New(socket)
	gateway.transport.ExpectContinueTimeout = 20 * time.Millisecond
	handlerDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		r.Body = &readSignal{ReadCloser: r.Body, started: readStarted}
		gateway.ServeHTTP(w, r)
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
		gateway.Close()
	}()
	client, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- ephemeral httptest certificate
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(700 * time.Millisecond))
	if _, err = fmt.Fprintf(client, "POST %s HTTP/1.1\r\nHost: test\r\nContent-Length: 1\r\nConnection: keep-alive\r\n\r\n", model.UploadPath); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("receiver remained blocked after writer %s: %v", mode, err)
	}
	gotBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	wantStatus := http.StatusBadGateway
	if mode == "split-response" {
		wantStatus = http.StatusUnauthorized
	}
	if response.StatusCode != wantStatus || readErr != nil || (mode == "split-response" && !strings.Contains(string(gotBody), "errore autorevole ritardato")) {
		t.Fatalf("final response: status=%d body=%q err=%v", response.StatusCode, gotBody, readErr)
	}
	_ = client.Close()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("receiver handler did not release its slot")
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if len(gateway.slots) != 0 {
		t.Fatalf("receiver slot retained: %d", len(gateway.slots))
	}
}

func TestLateEarlyResponseInterruptsBlockedClientBody(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "writer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	writer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		http.Error(w, "rejected after admission wait", http.StatusUnauthorized)
	})}
	writerDone := make(chan error, 1)
	go func() { writerDone <- writer.Serve(listener) }()
	gateway := New(socket)
	gateway.transport.ExpectContinueTimeout = 40 * time.Millisecond
	server := httptest.NewTLSServer(gateway)
	t.Cleanup(func() {
		server.Close()
		gateway.Close()
		_ = writer.Close()
		<-writerDone
	})

	conn, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- ephemeral httptest certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(700 * time.Millisecond))
	metadata, err := model.EncodeMetadata(model.Metadata{Description: "late rejection", OriginalName: "blocked.bin"})
	if err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: onlybackup.test\r\nAuthorization: Bearer printable-but-unknown\r\nContent-Type: application/octet-stream\r\nContent-Length: 1\r\n%s: %s\r\n%s: %s\r\n%s: obi_late_response_123456789\r\nConnection: keep-alive\r\n\r\n", model.UploadPath, model.MetadataHeader, metadata, model.DigestHeader, strings.Repeat("a", 64), model.IdempotencyHeader)
	started := time.Now()
	if _, err = io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(responseBody), "rejected after admission wait") {
		t.Fatalf("status=%d body=%s", response.StatusCode, responseBody)
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("late response timing: %v", elapsed)
	}
	if len(gateway.slots) != 0 {
		t.Fatalf("receiver slot retained: %d", len(gateway.slots))
	}
}

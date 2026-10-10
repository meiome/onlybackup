package localapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerificationEndpointsOutliveServerWriteDeadline(t *testing.T) {
	api, _ := apiFixture(t)
	now := api.Now()
	// Use a deliberately slow local command, rather than a huge or timing-sensitive
	// disk fixture, to exercise the actual server's short response deadline.
	api.Now = func() time.Time { time.Sleep(100 * time.Millisecond); return now }
	server := httptest.NewUnstartedServer(api.MaintenanceHandler())
	server.Config.WriteTimeout = 20 * time.Millisecond
	server.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, peerKey{}, Peer{UID: 1})
	}
	server.Start()
	defer server.Close()
	for _, path := range []string{"/v1/reconcile", "/v1/confirm"} {
		req, err := http.NewRequest("POST", server.URL+path, bytes.NewBufferString("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(protocolHeader, ProtocolVersion)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("server closed verification response at short deadline: %v", err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 409 {
			t.Fatalf("unexpected command result: %d", response.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", server.URL+"/v1/not-a-command", bytes.NewBufferString("{}"))
	req.Header.Set(protocolHeader, ProtocolVersion)
	response, err := server.Client().Do(req)
	if err == nil {
		response.Body.Close()
		t.Fatal("short command lost server write timeout")
	}
}

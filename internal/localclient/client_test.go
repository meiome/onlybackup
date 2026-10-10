package localclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerificationRequestsOutliveControlTimeoutAndRemainCancellable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set(protocolHeader, protocolVersion)
		w.WriteHeader(200)
	}))
	defer server.Close()
	c := &Client{http: &http.Client{Timeout: 20 * time.Millisecond}}
	// Route the fixed local URL to the test server without changing request paths.
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		request.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(request)
	})
	if err := c.Do(context.Background(), "GET", "/v1/snapshot", nil, nil); err == nil {
		t.Fatal("control timeout lost")
	}
	for _, path := range []string{"/v1/reconcile", "/v1/confirm"} {
		if err := c.Do(context.Background(), "POST", path, nil, nil); err != nil {
			t.Fatalf("whole-file request timed out: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := c.Do(ctx, "POST", path, nil, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller cancellation ignored: %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

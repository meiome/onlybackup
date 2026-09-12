// Package lifecycle coordinates server shutdown with the lifetime of admitted
// request handlers.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Tracker prevents new work from being admitted once shutdown starts and
// records work that must finish before process-owned resources can be closed.
type Tracker struct {
	mu       sync.Mutex
	active   int
	stopping bool
	drained  chan struct{}
}

func NewTracker() *Tracker {
	drained := make(chan struct{})
	close(drained)
	return &Tracker{drained: drained}
}

// Enter admits one unit of work. It returns false after Stop has been called.
func (t *Tracker) Enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopping {
		return false
	}
	if t.active == 0 {
		t.drained = make(chan struct{})
	}
	t.active++
	return true
}

// Leave marks one admitted unit of work as finished.
func (t *Tracker) Leave() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == 0 {
		panic("lifecycle: Leave without Enter")
	}
	t.active--
	if t.active == 0 {
		close(t.drained)
	}
}

// Stop prevents future admissions. Calls are idempotent.
func (t *Tracker) Stop() {
	t.mu.Lock()
	t.stopping = true
	t.mu.Unlock()
}

// Wait stops future admissions and waits for every admitted unit to finish.
func (t *Tracker) Wait() {
	t.mu.Lock()
	t.stopping = true
	drained := t.drained
	t.mu.Unlock()
	<-drained
}

// Handler tracks requests admitted before shutdown. Requests which reach the
// wrapper after shutdown has started are rejected without calling next.
func (t *Tracker) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !t.Enter() {
			w.Header().Set("Connection", "close")
			http.Error(w, "server in arresto", http.StatusServiceUnavailable)
			return
		}
		defer t.Leave()
		next.ServeHTTP(w, r)
	})
}

// Serve runs an HTTP server until it fails or ctx is cancelled. On
// cancellation it first stops handler admission, then attempts graceful
// shutdown. If the grace period expires, active connections are closed and
// Serve still waits for all admitted handlers before returning. The bool is
// true when connections had to be forced closed.
func Serve(ctx context.Context, server *http.Server, tracker *Tracker, grace time.Duration, serve func() error) (bool, error) {
	if grace <= 0 {
		return false, errors.New("durata arresto ordinato non valida")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve() }()

	select {
	case err := <-serveDone:
		tracker.Stop()
		closeErr := server.Close()
		tracker.Wait()
		return false, errors.Join(normalizeServeError(err), normalizeCloseError(closeErr))
	case <-ctx.Done():
	}

	tracker.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	forced := shutdownErr != nil
	var closeErr error
	if forced {
		closeErr = server.Close()
	}
	tracker.Wait()
	serveErr := <-serveDone

	if forced && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		shutdownErr = fmt.Errorf("arresto ordinato: %w", shutdownErr)
	} else {
		shutdownErr = nil
	}
	return forced, errors.Join(shutdownErr, normalizeCloseError(closeErr), normalizeServeError(serveErr))
}

func normalizeServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func normalizeCloseError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

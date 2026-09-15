// Package httpbody tracks and interrupts HTTP request bodies which are being
// forwarded to another service.
package httpbody

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Tracker records how much of a request body has been consumed. It remains an
// io.ReadCloser so transports can close the original body normally.
type Tracker struct {
	body io.ReadCloser
	read atomic.Int64
	eof  atomic.Bool
}

// Track replaces r.Body with a tracked wrapper and returns that wrapper.
func Track(r *http.Request) *Tracker {
	t := &Tracker{body: r.Body}
	r.Body = t
	return t
}

func (t *Tracker) Read(p []byte) (int, error) {
	if t.body == nil {
		t.eof.Store(true)
		return 0, io.EOF
	}
	n, err := t.body.Read(p)
	t.read.Add(int64(n))
	if err == io.EOF {
		t.eof.Store(true)
	}
	return n, err
}

func (t *Tracker) Close() error {
	if t.body == nil {
		return nil
	}
	return t.body.Close()
}

// Incomplete reports whether the declared body may still contain unread data.
func (t *Tracker) Incomplete(contentLength int64) bool {
	switch {
	case contentLength == 0:
		return false
	case contentLength > 0:
		return t.read.Load() < contentLength
	default:
		return !t.eof.Load()
	}
}

// Interrupt makes an outstanding body read return without waiting for the
// client. HTTP/1.x reuse is disabled before the expired deadline is installed,
// because an expired connection deadline must not escape into another request.
// ResponseController may be unsupported by in-memory ResponseWriters; closing
// the ReadCloser remains the interruption mechanism for those callers.
func Interrupt(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 {
		w.Header().Set("Connection", "close")
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	if r.Body != nil {
		_ = r.Body.Close()
	}
}

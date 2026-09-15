// Package gateway exposes only upload forwarding. It has no database or archive dependency.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/meiome/onlybackup/internal/httpbody"
	"github.com/meiome/onlybackup/internal/model"
)

type Gateway struct {
	transport *http.Transport
	slots     chan struct{}
}

const writerAdmissionTimeout = 11 * time.Second
const finalResponseTimeout = 5 * time.Second
const maxResponseBytes = 64 << 10

type observedConn struct {
	net.Conn
	failed chan struct{}
	once   sync.Once
}

func (c *observedConn) signal(err error) {
	if err != nil {
		c.once.Do(func() { close(c.failed) })
	}
}

func (c *observedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.signal(err)
	return n, err
}

func (c *observedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.signal(err)
	return n, err
}

type roundTripResult struct {
	response *http.Response
	err      error
}

func New(socket string) *Gateway {
	return &Gateway{transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			if err != nil {
				return nil, err
			}
			return &observedConn{Conn: conn, failed: make(chan struct{})}, nil
		},
		// Admission happens before the writer reads the body. The longer
		// expect-continue window lets authentication and SQLite contention finish
		// before the receiver starts consuming a blocked client body.
		DisableCompression: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 32, IdleConnTimeout: 30 * time.Second, ExpectContinueTimeout: writerAdmissionTimeout, ResponseHeaderTimeout: 2 * time.Hour,
	}, slots: make(chan struct{}, 32)}
}
func (g *Gateway) Close() { g.transport.CloseIdleConnections() }
func respond(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := httpbody.Track(r)
	if !model.ValidUploadPath(r.URL.Path) || r.URL.RawQuery != "" {
		httpbody.Interrupt(w, r)
		respond(w, 404, "operazione inesistente")
		return
	}
	if r.Method != http.MethodPost {
		httpbody.Interrupt(w, r)
		respond(w, 405, "è consentito soltanto l'invio")
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		httpbody.Interrupt(w, r)
		respond(w, 503, "server occupato")
		return
	}
	if r.ContentLength <= 0 {
		httpbody.Interrupt(w, r)
		respond(w, 411, "richiesto un file non vuoto con dimensione nota")
		return
	}
	outboundCtx, cancelOutbound := context.WithCancel(r.Context())
	defer cancelOutbound()
	connReady := make(chan *observedConn, 1)
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if conn, ok := info.Conn.(*observedConn); ok {
			connReady <- conn
		}
	}}
	outboundCtx = httptrace.WithClientTrace(outboundCtx, trace)
	req, err := http.NewRequestWithContext(outboundCtx, http.MethodPost, "http://writer"+r.URL.Path, r.Body)
	if err != nil {
		httpbody.Interrupt(w, r)
		respond(w, 400, "richiesta non valida")
		return
	}
	req.ContentLength = r.ContentLength
	for _, name := range []string{"Authorization", "Content-Type", "Content-Encoding", model.MetadataHeader, model.DigestHeader, model.IdempotencyHeader} {
		if vs := r.Header.Values(name); len(vs) > 1 {
			httpbody.Interrupt(w, r)
			respond(w, 400, "header duplicato")
			return
		}
		req.Header.Set(name, r.Header.Get(name))
	}
	req.Header.Set("Expect", "100-continue")
	resultReady := make(chan roundTripResult, 1)
	go func() {
		response, roundTripErr := g.transport.RoundTrip(req)
		resultReady <- roundTripResult{response: response, err: roundTripErr}
	}()
	var result roundTripResult
	var outboundConn *observedConn
	select {
	case result = <-resultReady:
		select {
		case outboundConn = <-connReady:
		default:
		}
	case conn := <-connReady:
		outboundConn = conn
		select {
		case result = <-resultReady:
		case <-conn.failed:
			// A complete response can race with the peer's orderly close. Give
			// the transport a brief chance to publish its parsed headers before
			// treating the close as a body-forwarding deadlock.
			select {
			case result = <-resultReady:
				break
			case <-time.After(10 * time.Millisecond):
			}
			if result.response != nil || result.err != nil {
				break
			}
			// net/http can wait for its request-body goroutine even after the
			// peer has disappeared. Unblock that goroutine while RoundTrip is
			// still running, then collect its result before releasing the slot.
			if body.Incomplete(r.ContentLength) {
				httpbody.Interrupt(w, r)
			}
			select {
			case result = <-resultReady:
			case <-time.After(time.Second):
				cancelOutbound()
				_ = conn.Close()
				select {
				case result = <-resultReady:
				case <-time.After(time.Second):
					respond(w, http.StatusBadGateway, "esito non confermato: writer non disponibile")
					return
				}
			}
		}
	}
	response, err := result.response, result.err
	if err != nil {
		if body.Incomplete(r.ContentLength) {
			httpbody.Interrupt(w, r)
		}
		respond(w, 502, "esito non confermato: writer non disponibile")
		return
	}
	responseBodyReady := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		responseBodyReady <- struct {
			body []byte
			err  error
		}{data, readErr}
	}()
	var responseBody []byte
	select {
	case readResult := <-responseBodyReady:
		responseBody, err = readResult.body, readResult.err
	case <-time.After(finalResponseTimeout):
		cancelOutbound()
		_ = response.Body.Close()
		if outboundConn != nil {
			_ = outboundConn.Close()
		}
		select {
		case readResult := <-responseBodyReady:
			responseBody = readResult.body
		case <-time.After(time.Second):
		}
		err = errors.New("timeout risposta writer")
	}
	_ = response.Body.Close()
	if body.Incomplete(r.ContentLength) {
		httpbody.Interrupt(w, r)
	}
	if err != nil || len(responseBody) > maxResponseBytes {
		respond(w, http.StatusBadGateway, "esito non confermato: risposta writer non valida")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

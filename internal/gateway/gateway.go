// Package gateway exposes only upload forwarding. It has no database or archive dependency.
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

type Gateway struct {
	transport *http.Transport
	slots     chan struct{}
}

func New(socket string) *Gateway {
	return &Gateway{transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
		DisableCompression: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 32, IdleConnTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second, ResponseHeaderTimeout: 2 * time.Hour,
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
	if !model.ValidUploadPath(r.URL.Path) || r.URL.RawQuery != "" {
		respond(w, 404, "operazione inesistente")
		return
	}
	if r.Method != http.MethodPost {
		respond(w, 405, "è consentito soltanto l'invio")
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		respond(w, 503, "server occupato")
		return
	}
	if r.ContentLength <= 0 {
		respond(w, 411, "richiesto un file non vuoto con dimensione nota")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://writer"+r.URL.Path, r.Body)
	if err != nil {
		respond(w, 400, "richiesta non valida")
		return
	}
	req.ContentLength = r.ContentLength
	for _, name := range []string{"Authorization", "Content-Type", "Content-Encoding", model.MetadataHeader, model.DigestHeader, model.IdempotencyHeader} {
		if vs := r.Header.Values(name); len(vs) > 1 {
			respond(w, 400, "header duplicato")
			return
		}
		req.Header.Set(name, r.Header.Get(name))
	}
	req.Header.Set("Expect", "100-continue")
	response, err := g.transport.RoundTrip(req)
	if err != nil {
		respond(w, 502, "esito non confermato: writer non disponibile")
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	io.Copy(w, io.LimitReader(response.Body, 64<<10))
}

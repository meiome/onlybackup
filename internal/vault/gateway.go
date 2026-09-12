package vault

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

type Gateway struct {
	socket        string
	uploadTimeout time.Duration
	slots         chan struct{}
	dialer        net.Dialer
}

func NewGateway(socket string, uploadTimeout time.Duration) *Gateway {
	return &Gateway{socket: socket, uploadTimeout: uploadTimeout, slots: make(chan struct{}, maxHandlers), dialer: net.Dialer{Timeout: 5 * time.Second}}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !model.ValidUploadPath(r.URL.Path) || r.URL.RawQuery != "" {
		gatewayError(w, http.StatusNotFound, "operazione inesistente")
		return
	}
	if r.Method != http.MethodPost {
		gatewayError(w, http.StatusMethodNotAllowed, "è consentito soltanto l'invio")
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		gatewayError(w, http.StatusServiceUnavailable, "server occupato")
		return
	}
	for _, name := range []string{"Authorization", "Content-Type", "Content-Encoding", model.MetadataHeader, model.DigestHeader, model.IdempotencyHeader} {
		if len(r.Header.Values(name)) > 1 {
			gatewayError(w, http.StatusBadRequest, "header duplicato")
			return
		}
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || !model.ValidToken(strings.TrimPrefix(auth, "Bearer ")) {
		gatewayError(w, http.StatusUnauthorized, model.ErrUnauthorized.Error())
		return
	}
	if r.ContentLength <= 0 {
		gatewayError(w, http.StatusLengthRequired, "richiesto un file non vuoto con dimensione nota")
		return
	}
	if r.ContentLength > maxUploadBytes {
		gatewayError(w, http.StatusRequestEntityTooLarge, model.ErrSize.Error())
		return
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Encoding") != "" {
		gatewayError(w, http.StatusUnsupportedMediaType, "inviare il contenuto grezzo del file")
		return
	}
	metadata, err := model.DecodeMetadata(r.Header.Get(model.MetadataHeader))
	if err != nil {
		gatewayError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest := r.Header.Get(model.DigestHeader)
	if !model.ValidDigest(digest) {
		gatewayError(w, http.StatusBadRequest, "SHA-256 richiesto e non valido")
		return
	}
	idempotencyKey := r.Header.Get(model.IdempotencyHeader)
	if r.URL.Path == model.UploadPath && idempotencyKey == "" {
		gatewayError(w, http.StatusBadRequest, "chiave di idempotenza richiesta")
		return
	}
	if idempotencyKey != "" && !model.ValidIdempotencyKey(idempotencyKey) {
		gatewayError(w, http.StatusBadRequest, "chiave di idempotenza non valida")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.uploadTimeout)
	defer cancel()
	conn, err := g.dialer.DialContext(ctx, "unix", g.socket)
	if err != nil {
		gatewayError(w, http.StatusBadGateway, "esito non confermato: vault non disponibile")
		return
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	envelope := Envelope{Token: strings.TrimPrefix(auth, "Bearer "), Metadata: metadata, Size: r.ContentLength, SHA256: digest, IdempotencyKey: idempotencyKey}
	if err = writeFrame(conn, envelope, maxEnvelopeBytes); err != nil {
		gatewayError(w, http.StatusBadGateway, "esito non confermato: vault non disponibile")
		return
	}
	_, err = io.CopyN(conn, r.Body, r.ContentLength)
	if err != nil {
		_ = closeWrite(conn)
		if result, responseErr := readVaultResponse(conn, envelope); responseErr == nil {
			writeGatewayResponse(w, result)
			return
		}
		gatewayError(w, http.StatusBadRequest, "trasferimento incompleto")
		return
	}
	var extra [1]byte
	if n, readErr := r.Body.Read(extra[:]); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		if n != 0 {
			_, _ = conn.Write(extra[:n])
		}
		_ = closeWrite(conn)
		gatewayError(w, http.StatusBadRequest, "dimensione del trasferimento errata")
		return
	}
	if closeWrite(conn) != nil {
		gatewayError(w, http.StatusBadGateway, "esito non confermato: vault non disponibile")
		return
	}
	result, err := readVaultResponse(conn, envelope)
	if err != nil {
		gatewayError(w, http.StatusBadGateway, "esito non confermato: risposta vault non valida")
		return
	}
	writeGatewayResponse(w, result)
}

func closeWrite(conn net.Conn) error {
	closer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("la connessione non supporta la chiusura in scrittura")
	}
	return closer.CloseWrite()
}

func readVaultResponse(conn net.Conn, envelope Envelope) (response, error) {
	var result response
	if err := readFrame(conn, &result, maxResponseBytes); err != nil {
		return response{}, err
	}
	if !validResponse(result, envelope) {
		return response{}, errors.New("risposta vault non valida")
	}
	return result, nil
}

func writeGatewayResponse(w http.ResponseWriter, result response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.StatusCode)
	if result.Receipt != nil {
		_ = json.NewEncoder(w).Encode(result.Receipt)
	} else {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": result.Error})
	}
}

func validResponse(r response, envelope Envelope) bool {
	if r.StatusCode == http.StatusCreated {
		return r.Error == "" && r.Receipt != nil && model.ValidID(r.Receipt.ID) && r.Receipt.Status == "complete" &&
			r.Receipt.Size == envelope.Size && r.Receipt.SHA256 == envelope.SHA256 && r.Receipt.ReceivedAt != ""
	}
	return r.StatusCode >= 400 && r.StatusCode <= 599 && r.Receipt == nil && strings.TrimSpace(r.Error) != ""
}

func gatewayError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

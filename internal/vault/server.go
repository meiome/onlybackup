package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/meiome/onlybackup/internal/ingest"
	"github.com/meiome/onlybackup/internal/lifecycle"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

const (
	headerTimeout = 10 * time.Second
	writeTimeout  = time.Minute
	maxHandlers   = 32
)

type Server struct {
	writer        *ingest.Writer
	uploadTimeout time.Duration
	slots         chan struct{}

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closing  bool
	tracker  *lifecycle.Tracker
}

func NewServer(s *store.Store, state string, reserveFree int64, uploadTimeout time.Duration) *Server {
	return &Server{
		writer:        ingest.New(s, state, reserveFree),
		uploadTimeout: uploadTimeout,
		slots:         make(chan struct{}, maxHandlers),
		conns:         make(map[net.Conn]struct{}),
		tracker:       lifecycle.NewTracker(),
	}
}

func (s *Server) Reconcile() error { return s.writer.Reconcile() }

func (s *Server) Serve(listener net.Listener) error {
	s.mu.Lock()
	if s.listener != nil || s.closing {
		s.mu.Unlock()
		return errors.New("vault server già avviato o arrestato")
	}
	s.listener = listener
	s.mu.Unlock()

	for {
		conn, err := listener.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.slots <- struct{}{}:
		default:
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = writeFrame(conn, response{StatusCode: http.StatusServiceUnavailable, Error: "vault occupato"}, maxResponseBytes)
			_ = conn.Close()
			continue
		}
		if !s.tracker.Enter() {
			<-s.slots
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			s.tracker.Leave()
			<-s.slots
			_ = conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.tracker.Leave()
		<-s.slots
	}()

	_ = conn.SetReadDeadline(time.Now().Add(headerTimeout))
	var envelope Envelope
	if err := readFrame(conn, &envelope, maxEnvelopeBytes); err != nil {
		s.writeResponse(conn, response{StatusCode: http.StatusBadRequest, Error: "richiesta vault non valida"})
		return
	}
	if status, err := validateEnvelope(envelope); err != nil {
		s.writeResponse(conn, response{StatusCode: status, Error: err.Error()})
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(s.uploadTimeout))

	uploadPath := model.LegacyUploadPath
	if envelope.IdempotencyKey != "" {
		uploadPath = model.UploadPath
	}
	r := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Path: uploadPath},
		Header:        make(http.Header),
		Body:          io.NopCloser(io.LimitReader(conn, envelope.Size+1)),
		ContentLength: envelope.Size,
	}
	r.Header.Set("Authorization", "Bearer "+envelope.Token)
	r.Header.Set("Content-Type", "application/octet-stream")
	metadata, err := model.EncodeMetadata(envelope.Metadata)
	if err != nil {
		s.writeResponse(conn, response{StatusCode: http.StatusBadRequest, Error: err.Error()})
		return
	}
	r.Header.Set(model.MetadataHeader, metadata)
	r.Header.Set(model.DigestHeader, envelope.SHA256)
	if envelope.IdempotencyKey != "" {
		r.Header.Set(model.IdempotencyHeader, envelope.IdempotencyKey)
	}

	w := newCaptureWriter()
	s.writer.ServeHTTP(w, r)
	result := responseFromWriter(w, envelope)
	if result.StatusCode == http.StatusCreated && result.Receipt != nil {
		// Verify the authoritative catalogue before producing the receipt. The
		// peer supplies no ID or profile and its size/hash claims are accepted
		// only after ingest has independently checked the body and committed.
		stored, lookupErr := s.writer.Store.Backup(result.Receipt.ID)
		if lookupErr != nil || stored.Status != "complete" || stored.Size != envelope.Size ||
			stored.SHA256 != envelope.SHA256 || !sameMetadata(stored.Metadata, envelope.Metadata) {
			log.Printf("vault receipt verification failed for %q: %v", result.Receipt.ID, lookupErr)
			result = response{StatusCode: http.StatusServiceUnavailable, Error: "esito non confermato"}
		}
	}
	s.writeResponse(conn, result)
}

func validateEnvelope(e Envelope) (int, error) {
	if !model.ValidToken(e.Token) {
		return http.StatusUnauthorized, model.ErrUnauthorized
	}
	if err := e.Metadata.Validate(); err != nil {
		return http.StatusBadRequest, err
	}
	if e.Size <= 0 {
		return http.StatusLengthRequired, errors.New("richiesto un file non vuoto con dimensione nota")
	}
	if e.Size > maxUploadBytes {
		return http.StatusRequestEntityTooLarge, model.ErrSize
	}
	if !model.ValidDigest(e.SHA256) {
		return http.StatusBadRequest, errors.New("SHA-256 richiesto e non valido")
	}
	if e.IdempotencyKey != "" && !model.ValidIdempotencyKey(e.IdempotencyKey) {
		return http.StatusBadRequest, errors.New("chiave di idempotenza non valida")
	}
	return 0, nil
}

func (s *Server) writeResponse(conn net.Conn, result response) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := writeFrame(conn, result, maxResponseBytes); err != nil {
		log.Printf("vault response failed: %v", err)
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.tracker.Stop()
		if s.listener != nil {
			_ = s.listener.Close()
		}
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.tracker.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func sameMetadata(a, b model.Metadata) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}

type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter       { return &captureWriter{header: make(http.Header)} }
func (w *captureWriter) Header() http.Header { return w.header }
func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(p) > maxResponseBytes {
		return 0, errors.New("risposta ingest troppo grande")
	}
	return w.body.Write(p)
}

func responseFromWriter(w *captureWriter, envelope Envelope) response {
	if w.status == http.StatusCreated {
		var receipt model.Receipt
		if err := decodeStrict(w.body.Bytes(), &receipt); err != nil || !model.ValidID(receipt.ID) ||
			receipt.Status != "complete" || receipt.Size != envelope.Size || receipt.SHA256 != envelope.SHA256 ||
			receipt.ReceivedAt == "" {
			return response{StatusCode: http.StatusServiceUnavailable, Error: "esito non confermato"}
		}
		return response{StatusCode: http.StatusCreated, Receipt: &receipt}
	}
	status := w.status
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := decodeStrict(w.body.Bytes(), &body); err != nil || strings.TrimSpace(body.Error) == "" {
		body.Error = "deposito non riuscito"
	}
	return response{StatusCode: status, Error: body.Error}
}

func decodeStrict(b []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON aggiuntivo")
	}
	return nil
}

// Package ingest implements the local deposit-only writer. No download or admin routes exist.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

type Writer struct {
	Store       *store.Store
	Root        string
	ReserveFree int64
	admissions  sync.Mutex
	slots       chan struct{}
	createFile  func(string) (stagingFile, error)
	linkFile    func(string, string) error
	removeFile  func(string) error
	syncDir     func(string) error
	complete    func(string, time.Time) error
}

type stagingFile interface {
	io.Writer
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

// ErrBusy reports exhaustion of the writer's process-wide handler slots.
var ErrBusy = errors.New("server occupato")

// Admission is a single, already-accounted authorization to transfer one
// backup. It may be consumed once or aborted; either path releases its slot.
type Admission struct {
	Backup   model.Backup
	writer   *Writer
	released sync.Once
}

func (u *Admission) release() {
	u.released.Do(func() { <-u.writer.slots })
}

// Abort records an admitted transfer as failed and releases its resources.
func (u *Admission) Abort(reason string) {
	if u.Backup.Status == "receiving" {
		if err := u.writer.Store.Fail(u.Backup.ID, reason); err != nil {
			log.Printf("mark failed %s: %v", u.Backup.ID, err)
		}
	}
	u.release()
}

func New(s *store.Store, root string, reserveFree int64) *Writer {
	return &Writer{
		Store: s, Root: root, ReserveFree: reserveFree,
		slots: make(chan struct{}, 32),
		createFile: func(path string) (stagingFile, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		},
		linkFile: os.Link, removeFile: os.Remove, syncDir: SyncDir, complete: s.Complete,
	}
}
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func Error(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func (a *Writer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !model.ValidUploadPath(r.URL.Path) || r.URL.RawQuery != "" {
		Error(w, 404, "operazione inesistente")
		return
	}
	if r.Method != http.MethodPost {
		Error(w, 405, "è consentito soltanto l'invio")
		return
	}
	for _, name := range []string{"Authorization", "Content-Type", "Content-Encoding", model.MetadataHeader, model.DigestHeader, model.IdempotencyHeader} {
		if len(r.Header.Values(name)) > 1 {
			Error(w, 400, "header duplicato")
			return
		}
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || !model.ValidToken(strings.TrimPrefix(auth, "Bearer ")) {
		Error(w, 401, model.ErrUnauthorized.Error())
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if r.ContentLength <= 0 {
		Error(w, 411, "richiesto un file non vuoto con dimensione nota")
		return
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Encoding") != "" {
		Error(w, 415, "inviare il contenuto grezzo del file")
		return
	}
	m, err := model.DecodeMetadata(r.Header.Get(model.MetadataHeader))
	if err != nil {
		Error(w, 400, err.Error())
		return
	}
	digest := r.Header.Get(model.DigestHeader)
	if !model.ValidDigest(digest) {
		Error(w, 400, "SHA-256 richiesto e non valido")
		return
	}
	idempotencyKey := r.Header.Get(model.IdempotencyHeader)
	if r.URL.Path == model.UploadPath && idempotencyKey == "" {
		Error(w, 400, "chiave di idempotenza richiesta")
		return
	}
	if idempotencyKey != "" && !model.ValidIdempotencyKey(idempotencyKey) {
		Error(w, 400, "chiave di idempotenza non valida")
		return
	}
	upload, err := a.Admit(token, m, r.ContentLength, digest, idempotencyKey, time.Now())
	if err != nil {
		status, message := AdmissionError(err)
		Error(w, status, message)
		return
	}
	if upload.Backup.Status == "complete" {
		writeReceipt(w, upload.Backup.Receipt)
		return
	}
	a.Receive(w, r.Body, upload)
}

// AdmissionError maps authoritative admission failures to their public status.
func AdmissionError(err error) (int, string) {
	switch {
	case errors.Is(err, model.ErrUnauthorized):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, model.ErrQuota), errors.Is(err, model.ErrRate), errors.Is(err, model.ErrConcurrent):
		return http.StatusTooManyRequests, err.Error()
	case errors.Is(err, model.ErrSize):
		return http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, model.ErrDisk):
		return http.StatusInsufficientStorage, err.Error()
	case errors.Is(err, model.ErrIdempotencyConflict), errors.Is(err, model.ErrIdempotencyInProgress):
		return http.StatusConflict, err.Error()
	case errors.Is(err, ErrBusy):
		return http.StatusServiceUnavailable, err.Error()
	default:
		log.Printf("reservation failed: %v", err)
		return http.StatusServiceUnavailable, "impossibile prenotare lo spazio"
	}
}

// Admit authenticates and reserves a transfer before any body bytes are read.
// The attempt remains counted if the caller later aborts or disconnects.
func (a *Writer) Admit(token string, m model.Metadata, size int64, digest, idempotencyKey string, now time.Time) (*Admission, error) {
	select {
	case a.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	b, err := a.reserve(token, m, size, digest, idempotencyKey, now)
	if err != nil {
		<-a.slots
		return nil, err
	}
	upload := &Admission{Backup: b, writer: a}
	if b.Status == "complete" {
		upload.release()
	}
	return upload, nil
}

// Receive consumes the body of one admitted transfer and writes its final HTTP
// result. It is the writer's only save and finalize path.
func (a *Writer) Receive(w http.ResponseWriter, body io.Reader, upload *Admission) {
	b := upload.Backup
	defer upload.release()
	staging := filepath.Join(a.Root, "incoming", b.ID+".part")
	final := filepath.Join(a.Root, "backups", b.ID+".backup")
	published := false
	success := false
	defer func() {
		if !published {
			if err := os.Remove(staging); err != nil && !os.IsNotExist(err) {
				log.Printf("cleanup %s: %v", b.ID, err)
			}
			if err := a.Store.Fail(b.ID, "upload interrotto o non valido"); err != nil {
				log.Printf("mark failed %s: %v", b.ID, err)
			}
		}
		if published && !success {
			log.Printf("backup %s published but receipt unconfirmed; reconcile on restart", b.ID)
		}
	}()
	f, err := a.createFile(staging)
	if err != nil {
		storageError(w, "creazione del file temporaneo", err)
		return
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	n, calculatedDigest, readErr, writeErr := copyAndHash(f, body, b.Size+1)
	if writeErr != nil {
		storageError(w, "scrittura del backup", writeErr)
		return
	}
	if readErr != nil || n != b.Size {
		Error(w, 400, "trasferimento incompleto o dimensione errata")
		return
	}
	if calculatedDigest != b.SHA256 {
		Error(w, 422, "integrità del file non verificata")
		return
	}
	if err = f.Chmod(0400); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	closed = true
	if err == nil {
		err = closeErr
	}
	if err != nil {
		storageError(w, "salvataggio del backup", err)
		return
	}
	// Hard link publishes without replacing any existing filename. Both directories
	// must live on the same filesystem. No completed backup is ever removed here.
	if err = a.linkFile(staging, final); err != nil {
		storageError(w, "pubblicazione del backup", err)
		return
	}
	published = true
	if err = a.syncDir(filepath.Dir(final)); err != nil {
		storageError(w, "sincronizzazione del backup pubblicato", err)
		return
	}
	if err = a.removeFile(staging); err != nil {
		Error(w, 503, "esito non confermato")
		return
	}
	if err = a.syncDir(filepath.Dir(staging)); err != nil {
		storageError(w, "sincronizzazione dei temporanei", err)
		return
	}
	now := time.Now().UTC()
	if err = a.complete(b.ID, now); err != nil {
		log.Printf("complete %s: %v", b.ID, err)
		storageError(w, "persistenza del catalogo", err)
		return
	}
	success = true
	b.Status = "complete"
	b.ReceivedAt = now.Format(time.RFC3339Nano)
	writeReceipt(w, b.Receipt)
}

func copyAndHash(dst io.Writer, src io.Reader, limit int64) (int64, string, error, error) {
	h := sha256.New()
	lr := &io.LimitedReader{R: src, N: limit}
	buf := make([]byte, 128<<10)
	var total int64
	for lr.N > 0 {
		n, readErr := lr.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if written > 0 {
				_, _ = h.Write(buf[:written])
				total += int64(written)
			}
			if writeErr != nil {
				return total, hex.EncodeToString(h.Sum(nil)), nil, writeErr
			}
			if written != n {
				return total, hex.EncodeToString(h.Sum(nil)), nil, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, hex.EncodeToString(h.Sum(nil)), nil, nil
			}
			return total, hex.EncodeToString(h.Sum(nil)), readErr, nil
		}
		if n == 0 {
			return total, hex.EncodeToString(h.Sum(nil)), io.ErrNoProgress, nil
		}
	}
	return total, hex.EncodeToString(h.Sum(nil)), nil, nil
}

func storageError(w http.ResponseWriter, operation string, err error) {
	status := http.StatusServiceUnavailable
	message := "esito non confermato: " + operation + " non riuscita"
	var sqliteErr sqlite3.Error
	if errors.Is(err, syscall.ENOSPC) || (errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrFull) {
		status = http.StatusInsufficientStorage
		message = "spazio disco esaurito durante " + operation
	}
	Error(w, status, message)
}
func writeReceipt(w http.ResponseWriter, receipt model.Receipt) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(receipt)
}
func (a *Writer) reserve(token string, m model.Metadata, size int64, digest, idempotencyKey string, now time.Time) (model.Backup, error) {
	a.admissions.Lock()
	defer a.admissions.Unlock()
	return a.Store.ReserveIdempotentChecked(token, m, size, digest, idempotencyKey, now, a.checkDisk)
}

func (a *Writer) checkDisk(reservedBytes int64) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(a.Root, "backups"), &fs); err != nil {
		return err
	}
	if fs.Bsize <= 0 || reservedBytes < 0 || a.ReserveFree < 0 {
		return errors.New("contabilita spazio disco non valida")
	}
	blockSize := uint64(fs.Bsize)
	if fs.Bavail > ^uint64(0)/blockSize {
		return errors.New("spazio disco non rappresentabile")
	}
	free := fs.Bavail * blockSize
	reserveFree := uint64(a.ReserveFree)
	if free < reserveFree || uint64(reservedBytes) > free-reserveFree {
		return model.ErrDisk
	}
	return nil
}

// Reconcile must run only while holding the exclusive writer lock, before listening.
// A finalized file survives uncertain DB commits. Incomplete staging files are discarded.
func (a *Writer) Reconcile() error {
	pending, err := a.Store.Pending()
	if err != nil {
		return err
	}
	for _, b := range pending {
		if !model.ValidID(b.ID) {
			return errors.New("identificativo non valido nel catalogo")
		}
		final := filepath.Join(a.Root, "backups", b.ID+".backup")
		temp := filepath.Join(a.Root, "incoming", b.ID+".part")
		info, err := os.Lstat(final)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() != b.Size {
				return fmt.Errorf("backup %s da verificare manualmente: file finale anomalo", b.ID)
			}
			if err = Verify(final, b.Size, b.SHA256); err != nil {
				return fmt.Errorf("backup %s da verificare manualmente: %w", b.ID, err)
			}
			if err = SyncDir(filepath.Dir(final)); err != nil {
				return err
			}
			if err = a.Store.Complete(b.ID, time.Now()); err != nil {
				return err
			}
		} else if os.IsNotExist(err) {
			if err = a.Store.Fail(b.ID, "invio interrotto dal riavvio"); err != nil {
				return err
			}
		} else {
			return err
		}
		if err = os.Remove(temp); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// A previous reconciliation may have committed Complete/Fail and then
	// stopped before unlinking its staging file. At this point the exclusive
	// writer lock is held and no uploads are running, so every correctly named
	// staging file is orphaned. Unknown files are left for manual inspection.
	entries, err := os.ReadDir(filepath.Join(a.Root, "incoming"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(name, ".part")
		if id == name || !model.ValidID(id) {
			continue
		}
		path := filepath.Join(a.Root, "incoming", name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("temporaneo %s anomalo: è una directory", name)
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return SyncDir(filepath.Join(a.Root, "incoming"))
}
func Verify(path string, size int64, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("dimensione o SHA-256 non corrispondenti")
	}
	return nil
}

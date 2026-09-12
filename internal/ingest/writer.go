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

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

type Writer struct {
	Store       *store.Store
	Root        string
	ReserveFree int64
	admissions  sync.Mutex
	slots       chan struct{}
}

func New(s *store.Store, root string, reserveFree int64) *Writer {
	return &Writer{Store: s, Root: root, ReserveFree: reserveFree, slots: make(chan struct{}, 32)}
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
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		Error(w, 503, "server occupato")
		return
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
	b, err := a.reserve(token, m, r.ContentLength, digest, idempotencyKey)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrUnauthorized):
			Error(w, 401, err.Error())
		case errors.Is(err, model.ErrQuota), errors.Is(err, model.ErrRate), errors.Is(err, model.ErrConcurrent):
			Error(w, 429, err.Error())
		case errors.Is(err, model.ErrSize):
			Error(w, 413, err.Error())
		case errors.Is(err, model.ErrDisk):
			Error(w, 507, err.Error())
		case errors.Is(err, model.ErrIdempotencyConflict), errors.Is(err, model.ErrIdempotencyInProgress):
			Error(w, 409, err.Error())
		default:
			log.Printf("reservation failed: %v", err)
			Error(w, 503, "impossibile prenotare lo spazio")
		}
		return
	}
	if b.Status == "complete" {
		writeReceipt(w, b.Receipt)
		return
	}
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
	f, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		Error(w, 507, "impossibile creare il file temporaneo")
		return
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(r.Body, b.Size+1), make([]byte, 128<<10))
	if err != nil || n != b.Size {
		Error(w, 400, "trasferimento incompleto o dimensione errata")
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
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
		Error(w, 507, "salvataggio del file non riuscito")
		return
	}
	// Hard link publishes without replacing any existing filename. Both directories
	// must live on the same filesystem. No completed backup is ever removed here.
	if err = os.Link(staging, final); err != nil {
		Error(w, 507, "finalizzazione del backup non riuscita")
		return
	}
	published = true
	if err = SyncDir(filepath.Dir(final)); err != nil {
		Error(w, 503, "esito non confermato")
		return
	}
	if err = os.Remove(staging); err != nil {
		Error(w, 503, "esito non confermato")
		return
	}
	if err = SyncDir(filepath.Dir(staging)); err != nil {
		Error(w, 503, "esito non confermato")
		return
	}
	now := time.Now().UTC()
	if err = a.Store.Complete(b.ID, now); err != nil {
		log.Printf("complete %s: %v", b.ID, err)
		Error(w, 503, "esito non confermato")
		return
	}
	success = true
	b.Status = "complete"
	b.ReceivedAt = now.Format(time.RFC3339Nano)
	writeReceipt(w, b.Receipt)
}
func writeReceipt(w http.ResponseWriter, receipt model.Receipt) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(receipt)
}
func (a *Writer) reserve(token string, m model.Metadata, size int64, digest, idempotencyKey string) (model.Backup, error) {
	a.admissions.Lock()
	defer a.admissions.Unlock()
	// First authenticate/reserve transactionally; free-space checks then include
	// this upload and all other outstanding reservations (conservative accounting).
	b, err := a.Store.ReserveIdempotent(token, m, size, digest, idempotencyKey, time.Now())
	if err != nil {
		return b, err
	}
	if b.Status == "complete" {
		return b, nil
	}
	var fs syscall.Statfs_t
	err = syscall.Statfs(filepath.Join(a.Root, "backups"), &fs)
	if err != nil {
		a.Store.Fail(b.ID, "spazio disco non verificabile")
		return model.Backup{}, err
	}
	pending, err := a.Store.ReservedBytes()
	if err != nil {
		a.Store.Fail(b.ID, "prenotazioni non verificabili")
		return model.Backup{}, err
	}
	free := uint64(fs.Bavail) * uint64(fs.Bsize)
	if free < uint64(a.ReserveFree) || uint64(pending) > free-uint64(a.ReserveFree) {
		a.Store.Fail(b.ID, "spazio disco insufficiente")
		return model.Backup{}, model.ErrDisk
	}
	return b, nil
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

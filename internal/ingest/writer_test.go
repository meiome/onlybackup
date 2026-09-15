package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

type failingStagingFile struct {
	stagingFile
	err error
}

func (f failingStagingFile) Write([]byte) (int, error) { return 0, f.err }

type blockingSyncFile struct {
	stagingFile
	entered chan struct{}
	release chan struct{}
}

func (f blockingSyncFile) Sync() error {
	close(f.entered)
	<-f.release
	return f.stagingFile.Sync()
}

func fixture(t *testing.T) (*Writer, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.PutProfile(model.Profile{Name: "test", TotalBytes: 1024, MaxBackupBytes: 1024, UploadsPerDay: 100, Concurrent: 4}); err != nil {
		t.Fatal(err)
	}
	k, token, err := s.CreateKey("cliente", "test")
	if err != nil {
		t.Fatal(err)
	}
	return New(s, root, 0), k.ID, token
}
func request(token string, data []byte) *http.Request {
	r := httptest.NewRequest("POST", model.UploadPath, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/octet-stream")
	metadata, _ := model.EncodeMetadata(model.Metadata{Description: "esportazione database", OriginalName: "database.EXE"})
	r.Header.Set(model.MetadataHeader, metadata)
	h := sha256.Sum256(data)
	r.Header.Set(model.DigestHeader, hex.EncodeToString(h[:]))
	idempotencyKey, _ := model.NewIdempotencyKey()
	r.Header.Set(model.IdempotencyHeader, idempotencyKey)
	return r
}
func TestDepositDuplicateNamesAndForbiddenOperations(t *testing.T) {
	a, key, token := fixture(t)
	data := []byte("backup completo")
	var ids []string
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, request(token, data))
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		var receipt model.Receipt
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, receipt.ID)
		path := filepath.Join(a.Root, "backups", receipt.ID+".backup")
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal(string(got), err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0400 {
			t.Fatal(info.Mode())
		}
		b, _ := a.Store.Backup(receipt.ID)
		if b.OriginalName != "database.EXE" {
			t.Fatal(b)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("overwritten backup")
	}
	for _, method := range []string{"GET", "DELETE", "PUT", "PATCH", "HEAD", "OPTIONS"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, model.UploadPath, nil)
		a.ServeHTTP(w, r)
		if w.Code != 405 {
			t.Fatal(method, w.Code)
		}
	}
	for _, path := range []string{"/v1/receipts", "/admin", "/v1/backups/" + ids[0]} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	q, _ := a.Store.Quota(key, time.Now())
	if q.Used != int64(2*len(data)) {
		t.Fatal(q)
	}
	files, _ := os.ReadDir(filepath.Join(a.Root, "incoming"))
	if len(files) != 0 {
		t.Fatal(files)
	}
}
func TestIdempotentDepositReturnsOriginalReceipt(t *testing.T) {
	a, key, token := fixture(t)
	data := []byte("backup idempotente")
	idempotencyKey, err := model.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	deposit := func(body []byte) (int, model.Receipt) {
		r := request(token, body)
		r.Header.Set(model.IdempotencyHeader, idempotencyKey)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		var receipt model.Receipt
		_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		return w.Code, receipt
	}
	firstStatus, first := deposit(data)
	secondStatus, second := deposit(data)
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated || first.ID == "" || second != first {
		t.Fatalf("idempotent receipts: %d %+v; %d %+v", firstStatus, first, secondStatus, second)
	}
	if status, _ := deposit([]byte("contenuto diverso")); status != http.StatusConflict {
		t.Fatalf("changed replay status: %d", status)
	}
	files, err := os.ReadDir(filepath.Join(a.Root, "backups"))
	if err != nil || len(files) != 1 {
		t.Fatalf("idempotent files: %v %v", files, err)
	}
	q, err := a.Store.Quota(key, time.Now())
	if err != nil || q.Used != int64(len(data)) || q.Attempts24h != 1 {
		t.Fatal(q, err)
	}
}

func TestV2RequiresIdempotencyAndV1RemainsCompatible(t *testing.T) {
	a, _, token := fixture(t)
	data := []byte("backup compatibile")
	v2 := request(token, data)
	v2.Header.Del(model.IdempotencyHeader)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, v2)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("v2 without idempotency: %d %s", w.Code, w.Body.String())
	}
	v1 := request(token, data)
	v1.URL.Path = model.LegacyUploadPath
	v1.Header.Del(model.IdempotencyHeader)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, v1)
	if w.Code != http.StatusCreated {
		t.Fatalf("v1 compatibility: %d %s", w.Code, w.Body.String())
	}
}
func TestInvalidUploadsNeverComplete(t *testing.T) {
	for _, kind := range []string{"wrong-key", "short", "digest", "metadata", "idempotency", "unknown-size", "too-large", "disk"} {
		t.Run(kind, func(t *testing.T) {
			a, key, token := fixture(t)
			r := request(token, []byte("12345"))
			switch kind {
			case "wrong-key":
				r.Header.Set("Authorization", "Bearer bad")
			case "short":
				r.ContentLength = 9
			case "digest":
				r.Header.Set(model.DigestHeader, strings.Repeat("0", 64))
			case "metadata":
				r.Header.Set(model.MetadataHeader, "bad")
			case "idempotency":
				r.Header.Set(model.IdempotencyHeader, "bad")
			case "unknown-size":
				r.ContentLength = -1
			case "too-large":
				r.ContentLength = 1025
			case "disk":
				a.ReserveFree = 1 << 60
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Fatal(w.Code, w.Body.String())
			}
			files, _ := os.ReadDir(filepath.Join(a.Root, "backups"))
			if len(files) != 0 {
				t.Fatal(files)
			}
			temp, _ := os.ReadDir(filepath.Join(a.Root, "incoming"))
			if len(temp) != 0 {
				t.Fatal(temp)
			}
			q, _ := a.Store.Quota(key, time.Now())
			if q.Used != 0 || q.Reserved != 0 {
				t.Fatal(q)
			}
			if kind == "disk" && q.Attempts24h != 0 {
				t.Fatalf("disk rejection consumed an upload attempt: %+v", q)
			}
		})
	}
}
func TestCrashReconciliation(t *testing.T) {
	a, _, token := fixture(t)
	data := []byte("durable")
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	m := model.Metadata{Description: "db", OriginalName: "db.sql"}
	b, err := a.Store.Reserve(token, m, int64(len(data)), digest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(a.Root, "backups", b.ID+".backup")
	if err = os.WriteFile(final, data, 0400); err != nil {
		t.Fatal(err)
	}
	c, err := a.Store.Reserve(token, m, int64(len(data)), digest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(a.Root, "incoming", c.ID+".part")
	os.WriteFile(temp, []byte("part"), 0600)
	if err = a.Reconcile(); err != nil {
		t.Fatal(err)
	}
	b, _ = a.Store.Backup(b.ID)
	c, _ = a.Store.Backup(c.ID)
	if b.Status != "complete" || c.Status != "failed" {
		t.Fatal(b, c)
	}
	if _, err = os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err = os.Stat(final); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the catalog transition but before staging cleanup.
	if err = os.WriteFile(temp, []byte("left by interrupted reconciliation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("orphan staging file survived repeated reconciliation", err)
	}
	if got, err := os.ReadFile(final); err != nil || !bytes.Equal(got, data) {
		t.Fatal("completed backup changed during cleanup", string(got), err)
	}
	if err = a.Reconcile(); err != nil {
		t.Fatal("reconciliation is not repeatable", err)
	}
}
func TestCorruptFinalIsPreservedAndBlocksStartup(t *testing.T) {
	a, _, token := fixture(t)
	b, err := a.Store.Reserve(token, model.Metadata{Description: "db", OriginalName: "x"}, 3, strings.Repeat("a", 64), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.Root, "backups", b.ID+".backup")
	os.WriteFile(path, []byte("bad"), 0400)
	if err = a.Reconcile(); err == nil {
		t.Fatal("corruption ignored")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("corrupt final removed", err)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestReadFailureReleasesReservation(t *testing.T) {
	a, key, token := fixture(t)
	r := request(token, []byte("12345"))
	r.Body = io.NopCloser(brokenReader{})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	q, _ := a.Store.Quota(key, time.Now())
	if q.Reserved != 0 || q.Active != 0 {
		t.Fatal(q)
	}
}

func TestIncompleteAdmittedUploadNeverSucceeds(t *testing.T) {
	a, key, token := fixture(t)
	data := []byte("backup incompleto")
	r := request(token, data)
	upload, err := a.Admit(token, model.Metadata{Description: "esportazione database", OriginalName: "database.EXE"}, int64(len(data)), r.Header.Get(model.DigestHeader), r.Header.Get(model.IdempotencyHeader), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Receive(w, bytes.NewReader(data[:len(data)-1]), upload)
	if w.Code == http.StatusCreated {
		t.Fatalf("upload incompleto confermato: %s", w.Body.String())
	}
	q, err := a.Store.Quota(key, time.Now())
	if err != nil || q.Active != 0 || q.Reserved != 0 || q.Attempts24h != 1 {
		t.Fatalf("risorse dopo upload incompleto: %+v %v", q, err)
	}
}

func TestDiskFullDuringCopyReturns507AndCleansReservation(t *testing.T) {
	a, key, token := fixture(t)
	originalCreate := a.createFile
	a.createFile = func(path string) (stagingFile, error) {
		file, err := originalCreate(path)
		if err != nil {
			return nil, err
		}
		return failingStagingFile{stagingFile: file, err: syscall.ENOSPC}, nil
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, request(token, []byte("contenuto")))
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "spazio disco esaurito durante scrittura") {
		t.Fatalf("errore copia: %d %s", w.Code, w.Body.String())
	}
	q, err := a.Store.Quota(key, time.Now())
	if err != nil || q.Active != 0 || q.Reserved != 0 || q.Attempts24h != 1 {
		t.Fatalf("risorse dopo disco pieno: %+v %v", q, err)
	}
	if files, _ := os.ReadDir(filepath.Join(a.Root, "incoming")); len(files) != 0 {
		t.Fatalf("temporaneo non rimosso: %v", files)
	}
}

func TestFinalPathCollisionPreservesExistingFile(t *testing.T) {
	a, key, token := fixture(t)
	data := []byte("nuovo backup")
	digest := sha256.Sum256(data)
	upload, err := a.Admit(token, model.Metadata{Description: "collisione", OriginalName: "db.sql"}, int64(len(data)), hex.EncodeToString(digest[:]), "obi_collision_1234567890", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(a.Root, "backups", upload.Backup.ID+".backup")
	existing := []byte("contenuto preesistente intatto")
	if err = os.WriteFile(final, existing, 0400); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Receive(w, bytes.NewReader(data), upload)
	if w.Code < 400 || w.Code == http.StatusCreated {
		t.Fatalf("collisione accettata: %d %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, existing) {
		t.Fatalf("file preesistente modificato: %q %v", got, err)
	}
	q, err := a.Store.Quota(key, time.Now())
	if err != nil || q.Active != 0 || q.Reserved != 0 || q.Attempts24h != 1 {
		t.Fatalf("risorse dopo collisione: %+v %v", q, err)
	}
}

func TestDiskFullDuringFinalizationReturns507(t *testing.T) {
	a, key, token := fixture(t)
	a.linkFile = func(string, string) error { return syscall.ENOSPC }
	w := httptest.NewRecorder()
	a.ServeHTTP(w, request(token, []byte("contenuto")))
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "spazio disco esaurito durante pubblicazione") {
		t.Fatalf("errore finalizzazione: %d %s", w.Code, w.Body.String())
	}
	q, err := a.Store.Quota(key, time.Now())
	if err != nil || q.Active != 0 || q.Reserved != 0 || q.Attempts24h != 1 {
		t.Fatalf("risorse dopo finalizzazione fallita: %+v %v", q, err)
	}
}

func TestPublishedBackupSurvivesCatalogueFailure(t *testing.T) {
	a, _, token := fixture(t)
	a.complete = func(string, time.Time) error { return syscall.ENOSPC }
	data := []byte("pubblicato prima del catalogo")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, request(token, data))
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), "persistenza del catalogo") {
		t.Fatalf("errore catalogo: %d %s", w.Code, w.Body.String())
	}
	pending, err := a.Store.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("record pending: %+v %v", pending, err)
	}
	final := filepath.Join(a.Root, "backups", pending[0].ID+".backup")
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("backup pubblicato perso: %q %v", got, err)
	}
	a.complete = a.Store.Complete
	if err = a.Reconcile(); err != nil {
		t.Fatal(err)
	}
	stored, err := a.Store.Backup(pending[0].ID)
	if err != nil || stored.Status != "complete" {
		t.Fatalf("riconciliazione: %+v %v", stored, err)
	}
}

func TestCompleteBodyFinalizesAfterRequestCancellation(t *testing.T) {
	a, _, token := fixture(t)
	data := []byte("corpo già ricevuto")
	r := request(token, data)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("finalizzazione cancellata col contesto HTTP: %d %s", w.Code, w.Body.String())
	}
	var receipt model.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(a.Root, "backups", receipt.ID+".backup"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("backup non finalizzato: %q %v", got, err)
	}
}

func TestClientDisconnectAfterVerificationStillFinalizes(t *testing.T) {
	a, _, token := fixture(t)
	data := []byte("corpo verificato prima della disconnessione")
	entered := make(chan struct{})
	release := make(chan struct{})
	originalCreate := a.createFile
	a.createFile = func(path string) (stagingFile, error) {
		file, err := originalCreate(path)
		if err != nil {
			return nil, err
		}
		return blockingSyncFile{stagingFile: file, entered: entered, release: release}, nil
	}
	server := httptest.NewServer(a)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := request(token, data)
	r.URL.Scheme = "http"
	r.URL.Host = server.Listener.Addr().String()
	r.Host = server.Listener.Addr().String()
	r.RequestURI = ""
	writeDone := make(chan error, 1)
	go func() { writeDone <- r.Write(conn) }()
	select {
	case err = <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("invio del corpo bloccato")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer non ha verificato il corpo")
	}
	_ = conn.Close()
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		backups, listErr := a.Store.Backups("", 10)
		if listErr == nil && len(backups) == 1 && backups[0].Status == "complete" {
			got, readErr := os.ReadFile(filepath.Join(a.Root, "backups", backups[0].ID+".backup"))
			if readErr != nil || !bytes.Equal(got, data) {
				t.Fatalf("backup finalizzato: %q %v", got, readErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("finalizzazione non completata dopo la disconnessione: %v %v", backups, listErr)
		}
		time.Sleep(time.Millisecond)
	}
}

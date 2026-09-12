package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

func fixture(t *testing.T) (*Writer, string, string) {
	t.Helper()
	root := t.TempDir()
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

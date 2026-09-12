package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
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

type vaultFixture struct {
	root      string
	socket    string
	token     string
	keyID     string
	store     *store.Store
	server    *Server
	serveDone chan error
}

func newVaultFixture(t *testing.T, profile model.Profile) *vaultFixture {
	t.Helper()
	root := t.TempDir()
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalogue, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = catalogue.PutProfile(profile); err != nil {
		catalogue.Close()
		t.Fatal(err)
	}
	key, token, err := catalogue.CreateKey("writer non fidato", profile.Name)
	if err != nil {
		catalogue.Close()
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "vault.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		catalogue.Close()
		t.Fatal(err)
	}
	server := NewServer(catalogue, root, 0, 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	f := &vaultFixture{root: root, socket: socket, token: token, keyID: key.ID, store: catalogue, server: server, serveDone: done}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := catalogue.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return f
}

func testProfile(total int64) model.Profile {
	return model.Profile{Name: "vault-test", TotalBytes: total, MaxBackupBytes: total, UploadsPerDay: 100, Concurrent: 4}
}

func testEnvelope(token string, data []byte) Envelope {
	digest := sha256.Sum256(data)
	return Envelope{
		Token: token,
		Metadata: model.Metadata{
			Description:   "backup protetto",
			OriginalName:  "database.sql.age",
			ContentFormat: model.ContentFormatAgeV1,
		},
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(digest[:]),
	}
}

func sendEnvelope(t *testing.T, socket string, envelope Envelope, body []byte) response {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = writeFrame(conn, envelope, maxEnvelopeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var result response
	if err = readFrame(conn, &result, maxResponseBytes); err != nil {
		t.Fatal(err)
	}
	return result
}

func sendJSON(t *testing.T, socket string, rawJSON string, body []byte) response {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(rawJSON)))
	if err = writeAll(conn, prefix[:]); err == nil {
		err = writeAll(conn, []byte(rawJSON))
	}
	if err == nil {
		err = writeAll(conn, body)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var result response
	if err = readFrame(conn, &result, maxResponseBytes); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGatewayUploadRoundTripAndAuthoritativeReceipt(t *testing.T) {
	f := newVaultFixture(t, testProfile(1<<20))
	data := []byte("contenuto cifrato di prova")
	envelope := testEnvelope(f.token, data)
	metadata, err := model.EncodeMetadata(envelope.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, model.UploadPath, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set(model.MetadataHeader, metadata)
	r.Header.Set(model.DigestHeader, envelope.SHA256)
	idempotencyKey, err := model.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(model.IdempotencyHeader, idempotencyKey)
	w := httptest.NewRecorder()
	NewGateway(f.socket, 3*time.Second).ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var receipt model.Receipt
	if err = json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if !model.ValidID(receipt.ID) || receipt.Size != int64(len(data)) || receipt.SHA256 != envelope.SHA256 {
		t.Fatal(receipt)
	}
	stored, err := os.ReadFile(filepath.Join(f.root, "backups", receipt.ID+".backup"))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatal(string(stored), err)
	}
	record, err := f.store.Backup(receipt.ID)
	if err != nil || record.Status != "complete" || record.KeyID != f.keyID || record.IdempotencyKey != idempotencyKey || !sameMetadata(record.Metadata, envelope.Metadata) {
		t.Fatal(record, err)
	}
	replayRequest := httptest.NewRequest(http.MethodPost, model.UploadPath, bytes.NewReader(data))
	replayRequest.Header = r.Header.Clone()
	replay := httptest.NewRecorder()
	NewGateway(f.socket, 3*time.Second).ServeHTTP(replay, replayRequest)
	var replayReceipt model.Receipt
	if replay.Code != http.StatusCreated || json.Unmarshal(replay.Body.Bytes(), &replayReceipt) != nil || replayReceipt != receipt {
		t.Fatalf("vault idempotent replay: %d %s", replay.Code, replay.Body.String())
	}
	files, err := os.ReadDir(filepath.Join(f.root, "backups"))
	if err != nil || len(files) != 1 {
		t.Fatalf("vault duplicate files: %v %v", files, err)
	}
}

func TestStrictEnvelopeRejectsCallerAuthorityAndAmbiguity(t *testing.T) {
	f := newVaultFixture(t, testProfile(1024))
	data := []byte("abc")
	valid, err := json.Marshal(testEnvelope(f.token, data))
	if err != nil {
		t.Fatal(err)
	}
	withoutBrace := strings.TrimSuffix(string(valid), "}")
	cases := map[string]string{
		"path":           withoutBrace + `,"path":"/etc/passwd"}`,
		"caller id":      withoutBrace + `,"id":"00000000000000000000000000000000"}`,
		"operation":      withoutBrace + `,"operation":"delete"}`,
		"duplicate size": withoutBrace + `,"size_bytes":999}`,
		"nested unknown": strings.Replace(string(valid), `"original_name":"database.sql.age"`, `"original_name":"database.sql.age","profile":"XL"`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			result := sendJSON(t, f.socket, raw, data)
			if result.StatusCode != http.StatusBadRequest || result.Receipt != nil {
				t.Fatal(result)
			}
		})
	}
	rows, err := f.store.Backups("", 100)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	files, err := os.ReadDir(filepath.Join(f.root, "backups"))
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
}

func TestAuthenticationLimitsAndExactBodyAreVaultDecisions(t *testing.T) {
	f := newVaultFixture(t, testProfile(8))
	data := []byte("12345678")

	badAuth := testEnvelope("printable-but-wrong", data)
	if result := sendEnvelope(t, f.socket, badAuth, data); result.StatusCode != http.StatusUnauthorized {
		t.Fatal(result)
	}

	short := testEnvelope(f.token, data)
	if result := sendEnvelope(t, f.socket, short, data[:3]); result.StatusCode != http.StatusBadRequest {
		t.Fatal(result)
	}
	extra := testEnvelope(f.token, []byte("abc"))
	if result := sendEnvelope(t, f.socket, extra, []byte("abcd")); result.StatusCode != http.StatusBadRequest {
		t.Fatal(result)
	}

	valid := sendEnvelope(t, f.socket, testEnvelope(f.token, data), data)
	if valid.StatusCode != http.StatusCreated || valid.Receipt == nil {
		t.Fatal(valid)
	}
	secondData := []byte("x")
	if result := sendEnvelope(t, f.socket, testEnvelope(f.token, secondData), secondData); result.StatusCode != http.StatusTooManyRequests {
		t.Fatal(result)
	}

	q, err := f.store.Quota(f.keyID, time.Now())
	if err != nil || q.Used != 8 || q.Reserved != 0 {
		t.Fatal(q, err)
	}
	files, err := os.ReadDir(filepath.Join(f.root, "backups"))
	if err != nil || len(files) != 1 || files[0].Name() != valid.Receipt.ID+".backup" {
		t.Fatal(files, err)
	}
}

func TestMalformedFramesCannotAlterCompletedBackup(t *testing.T) {
	f := newVaultFixture(t, testProfile(1024))
	data := []byte("backup da preservare")
	valid := sendEnvelope(t, f.socket, testEnvelope(f.token, data), data)
	if valid.StatusCode != http.StatusCreated {
		t.Fatal(valid)
	}
	path := filepath.Join(f.root, "backups", valid.Receipt.ID+".backup")

	conn, err := net.DialTimeout("unix", f.socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], maxEnvelopeBytes+1)
	if err = writeAll(conn, prefix[:]); err != nil {
		t.Fatal(err)
	}
	_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	var rejected response
	if err = readFrame(conn, &rejected, maxResponseBytes); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatal(rejected)
	}

	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatal(string(stored), err)
	}
	record, err := f.store.Backup(valid.Receipt.ID)
	if err != nil || record.Status != "complete" {
		t.Fatal(record, err)
	}
}

func TestGatewayHasOnlyUploadRoute(t *testing.T) {
	gateway := NewGateway(filepath.Join(t.TempDir(), "absent.sock"), time.Second)
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, model.UploadPath, http.StatusMethodNotAllowed},
		{http.MethodDelete, model.UploadPath, http.StatusMethodNotAllowed},
		{http.MethodPost, model.UploadPath + "/chosen-id", http.StatusNotFound},
		{http.MethodPost, "/admin", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

func gatewayRequest(token string, data []byte) *http.Request {
	envelope := testEnvelope(token, data)
	metadata, _ := model.EncodeMetadata(envelope.Metadata)
	r := httptest.NewRequest(http.MethodPost, model.UploadPath, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set(model.MetadataHeader, metadata)
	r.Header.Set(model.DigestHeader, envelope.SHA256)
	idempotencyKey, _ := model.NewIdempotencyKey()
	r.Header.Set(model.IdempotencyHeader, idempotencyKey)
	return r
}

func TestGatewayPreservesEarlyVaultRejectionForLargeBody(t *testing.T) {
	const total = int64(8 << 20)
	t.Run("revoked", func(t *testing.T) {
		f := newVaultFixture(t, testProfile(total))
		if err := f.store.Revoke(f.keyID); err != nil {
			t.Fatal(err)
		}
		data := bytes.Repeat([]byte("x"), int(total))
		w := httptest.NewRecorder()
		NewGateway(f.socket, 3*time.Second).ServeHTTP(w, gatewayRequest(f.token, data))
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), f.token) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("quota", func(t *testing.T) {
		f := newVaultFixture(t, testProfile(total))
		first := []byte("x")
		if got := sendEnvelope(t, f.socket, testEnvelope(f.token, first), first); got.StatusCode != http.StatusCreated {
			t.Fatal(got)
		}
		data := bytes.Repeat([]byte("y"), int(total))
		w := httptest.NewRecorder()
		NewGateway(f.socket, 3*time.Second).ServeHTTP(w, gatewayRequest(f.token, data))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		q, err := f.store.Quota(f.keyID, time.Now())
		if err != nil || q.Used != 1 || q.Reserved != 0 {
			t.Fatal(q, err)
		}
	})
}

func TestProtocolSizeCapPreventsLimitOverflow(t *testing.T) {
	gateway := NewGateway(filepath.Join(t.TempDir(), "absent.sock"), time.Second)
	r := gatewayRequest("valid-printable-token", []byte("x"))
	r.ContentLength = maxUploadBytes + 1
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestShutdownForcesIdleConnectionThenDrains(t *testing.T) {
	f := newVaultFixture(t, testProfile(1024))
	conn, err := net.DialTimeout("unix", f.socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Let Serve admit the connection and block on its frame prefix.
	if _, err = conn.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		f.server.mu.Lock()
		admitted := len(f.server.conns) == 1
		f.server.mu.Unlock()
		if admitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle connection was not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err = f.server.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err = conn.Read(b[:]); err == nil {
		t.Fatal("idle connection survived forced shutdown")
	}
	// Cleanup will call Shutdown again; it must remain idempotent.
}

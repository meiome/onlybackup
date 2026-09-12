package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/meiome/onlybackup/internal/cryptfile"
	"github.com/meiome/onlybackup/internal/model"
)

type capturedUpload struct {
	metadata       model.Metadata
	body           []byte
	digest         string
	idempotencyKey string
}

func uploadServer(t *testing.T) (*httptest.Server, string, <-chan capturedUpload, *atomic.Int32) {
	t.Helper()
	uploads := make(chan capturedUpload, 10)
	var requests atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		metadata, err := model.DecodeMetadata(r.Header.Get(model.MetadataHeader))
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		digest := r.Header.Get(model.DigestHeader)
		uploads <- capturedUpload{metadata: metadata, body: body, digest: digest, idempotencyKey: r.Header.Get(model.IdempotencyHeader)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(model.Receipt{ID: "00112233445566778899aabbccddeeff", Status: "complete", Size: int64(len(body)), SHA256: digest, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	srv := httptest.NewTLSServer(h)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, cert, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, caPath, uploads, &requests
}

func clientFixture(t *testing.T) (source, key string, plaintext []byte) {
	t.Helper()
	dir := t.TempDir()
	plaintext = bytes.Repeat([]byte("database row\n"), 10000)
	source = filepath.Join(dir, "dump.sql")
	key = filepath.Join(dir, "send.key")
	if err := os.WriteFile(source, plaintext, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("deposit-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return source, key, plaintext
}

func TestSendExplicitPlaintextAndEncryptedRoundTrip(t *testing.T) {
	source, key, plaintext := clientFixture(t)
	metadata := model.Metadata{Description: "database", OriginalName: "dump.sql"}

	t.Run("plaintext-explicit", func(t *testing.T) {
		srv, ca, uploads, _ := uploadServer(t)
		if _, err := Send(context.Background(), Options{URL: srv.URL, KeyFile: key, CAFile: ca, Plaintext: true}, source, metadata, nil); err != nil {
			t.Fatal(err)
		}
		got := <-uploads
		if got.metadata.ContentFormat != "" || !bytes.Equal(got.body, plaintext) || got.digest != digestBytes(plaintext) || !model.ValidIdempotencyKey(got.idempotencyKey) {
			t.Fatalf("plaintext upload mismatch: %+v", got.metadata)
		}
	})

	t.Run("age-v1", func(t *testing.T) {
		srv, ca, uploads, _ := uploadServer(t)
		dir := t.TempDir()
		identityFile := filepath.Join(dir, "identity")
		recipient, err := cryptfile.GenerateIdentity(identityFile)
		if err != nil {
			t.Fatal(err)
		}
		tempDir := filepath.Join(dir, "temporary")
		if err = os.Mkdir(tempDir, 0700); err != nil {
			t.Fatal(err)
		}
		o := Options{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: recipient, EncryptedTempDir: tempDir, EncryptedTempLimitBytes: 2 << 20}
		if _, err = Send(context.Background(), o, source, metadata, nil); err != nil {
			t.Fatal(err)
		}
		got := <-uploads
		if got.metadata.ContentFormat != model.ContentFormatAgeV1 || bytes.Equal(got.body, plaintext) || got.digest != digestBytes(got.body) || !model.ValidIdempotencyKey(got.idempotencyKey) {
			t.Fatalf("encrypted upload mismatch: %+v", got.metadata)
		}
		identities, err := cryptfile.ReadIdentities(identityFile)
		if err != nil {
			t.Fatal(err)
		}
		decrypted, err := age.Decrypt(bytes.NewReader(got.body), identities...)
		if err != nil {
			t.Fatal(err)
		}
		clear, err := io.ReadAll(decrypted)
		if err != nil || !bytes.Equal(clear, plaintext) {
			t.Fatal("encrypted upload did not decrypt to source", err)
		}
		entries, err := os.ReadDir(tempDir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("ciphertext temporary was not removed: %v %v", entries, err)
		}
	})
}

func TestReadSecretRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.key")
	link := filepath.Join(dir, "linked.key")
	if err := os.WriteFile(target, []byte("deposit-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink non disponibile: %v", err)
	}
	if _, err := ReadSecret(link); err == nil {
		t.Fatal("token letto attraverso un link simbolico")
	}
	if token, err := ReadSecret(target); err != nil || token != "deposit-token" {
		t.Fatalf("regular token: %q %v", token, err)
	}
}

func TestSendRetriesWithSameIdempotencyKey(t *testing.T) {
	source, key, plaintext := clientFixture(t)
	var requests atomic.Int32
	type attempt struct {
		body []byte
		key  string
	}
	attempts := make(chan attempt, 3)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		attempts <- attempt{body: body, key: r.Header.Get(model.IdempotencyHeader)}
		if n == 1 {
			panic(http.ErrAbortHandler)
		}
		digest := r.Header.Get(model.DigestHeader)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(model.Receipt{ID: "00112233445566778899aabbccddeeff", Status: "complete", Size: int64(len(body)), SHA256: digest, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Send(context.Background(), Options{URL: srv.URL, KeyFile: key, CAFile: caPath, Plaintext: true}, source, model.Metadata{Description: "database", OriginalName: "dump.sql"}, nil); err != nil {
		t.Fatal(err)
	}
	first, second := <-attempts, <-attempts
	if requests.Load() != 2 || !bytes.Equal(first.body, plaintext) || !bytes.Equal(second.body, plaintext) ||
		!model.ValidIdempotencyKey(first.key) || first.key != second.key {
		t.Fatalf("retry mismatch: requests=%d first=%q second=%q", requests.Load(), first.key, second.key)
	}
}

func TestEncryptionConfigurationFailuresNeverUpload(t *testing.T) {
	source, key, _ := clientFixture(t)
	metadata := model.Metadata{Description: "database", OriginalName: "dump.sql"}
	srv, ca, _, requests := uploadServer(t)
	dir := t.TempDir()
	identityFile := filepath.Join(dir, "identity")
	recipient, err := cryptfile.GenerateIdentity(identityFile)
	if err != nil {
		t.Fatal(err)
	}
	cases := []Options{
		{URL: srv.URL, KeyFile: key, CAFile: ca},
		{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: recipient, Plaintext: true},
		{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: "not-an-age-recipient"},
		{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: recipient, EncryptedTempLimitBytes: 100},
		{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: recipient, EncryptedTempDir: filepath.Join(dir, "absent")},
		{URL: srv.URL, KeyFile: key, CAFile: ca, Plaintext: true, EncryptedTempLimitBytes: 1024},
	}
	for i, options := range cases {
		if _, err = Send(context.Background(), options, source, metadata, nil); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", i)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Send(canceled, Options{URL: srv.URL, KeyFile: key, CAFile: ca, EncryptTo: recipient}, source, metadata, nil); err == nil {
		t.Fatal("canceled encrypted send unexpectedly succeeded")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("configuration failures made %d HTTP requests", got)
	}
}

func TestTLSRejectsUntrustedCertificate(t *testing.T) {
	source, key, _ := clientFixture(t)
	srv, _, _, requests := uploadServer(t)
	_, err := Send(context.Background(), Options{URL: srv.URL, KeyFile: key, Plaintext: true}, source, model.Metadata{Description: "database", OriginalName: "dump.sql"}, nil)
	if err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP handler reached despite untrusted certificate: %d", got)
	}
}

func TestRedirectDoesNotForwardCredential(t *testing.T) {
	source, key, _ := clientFixture(t)
	var targetRequests atomic.Int32
	var targetAuthorization atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.Header.Get("Authorization") != "" {
			targetAuthorization.Store(true)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", target.URL+model.UploadPath)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	caPath := filepath.Join(t.TempDir(), "redirect-ca.pem")
	ca := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw})...)
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Send(context.Background(), Options{URL: redirect.URL, KeyFile: key, CAFile: caPath, Plaintext: true}, source, model.Metadata{Description: "database", OriginalName: "dump.sql"}, nil)
	if err == nil {
		t.Fatal("redirect unexpectedly reported success")
	}
	if targetRequests.Load() != 0 || targetAuthorization.Load() {
		t.Fatalf("redirect target received request=%d authorization=%v", targetRequests.Load(), targetAuthorization.Load())
	}
}

func TestContextDeadlineCancelsReceiptWait(t *testing.T) {
	source, key, _ := clientFixture(t)
	var requests atomic.Int32
	handlerDone := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(handlerDone)
	}))
	defer srv.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, cert, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Send(ctx, Options{URL: srv.URL, KeyFile: key, CAFile: caPath, Plaintext: true}, source, model.Metadata{Description: "database", OriginalName: "dump.sql"}, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took too long: %s", elapsed)
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server request context was not canceled")
	}
	if requests.Load() != 1 {
		t.Fatalf("unexpected requests: %d", requests.Load())
	}
}

func TestMaxExpandedMetadataIsAcceptedExactly(t *testing.T) {
	source, key, _ := clientFixture(t)
	srv, ca, uploads, _ := uploadServer(t)
	// encoding/json expands '<' to six ASCII bytes (\u003c), exercising the
	// largest accepted rune counts after JSON and base64 expansion.
	metadata := model.Metadata{Description: strings.Repeat("<", 1000), OriginalName: strings.Repeat("<", 255)}
	if _, err := Send(context.Background(), Options{URL: srv.URL, KeyFile: key, CAFile: ca, Plaintext: true}, source, metadata, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-uploads; got.metadata != metadata {
		t.Fatal("maximum expanded metadata changed in transit")
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

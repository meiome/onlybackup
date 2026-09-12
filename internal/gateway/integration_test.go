package gateway_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/client"
	"github.com/meiome/onlybackup/internal/gateway"
	"github.com/meiome/onlybackup/internal/ingest"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

func TestTLSClientThroughUnixWriter(t *testing.T) {
	root, err := os.MkdirTemp("", "ob-wire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	state := filepath.Join(root, "state")
	if err = store.Init(state); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(state, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key, token, err := s.CreateKey("cliente", "XS")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "writer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	writer := &http.Server{Handler: ingest.New(s, state, 0)}
	go writer.Serve(listener)
	defer writer.Close()
	gatewayHandler := gateway.New(socket)
	defer gatewayHandler.Close()
	server := httptest.NewTLSServer(gatewayHandler)
	defer server.Close()
	ca := filepath.Join(root, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	keyFile := filepath.Join(root, "key")
	os.WriteFile(keyFile, []byte(token+"\n"), 0600)
	data := bytes.Repeat([]byte("database row\n"), 10000)
	file := filepath.Join(root, "db.sql")
	os.WriteFile(file, data, 0600)
	var progress bytes.Buffer
	o := client.Options{URL: server.URL, KeyFile: keyFile, CAFile: ca, Plaintext: true}
	receipt, err := client.Send(context.Background(), o, file, model.Metadata{Description: "backup integrato", OriginalName: "db.sql"}, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Size != int64(len(data)) || !strings.Contains(progress.String(), "Backup ricevuto e salvato") {
		t.Fatal(receipt, progress.String())
	}
	if strings.Count(progress.String(), "100.0%") != 1 || !strings.Contains(progress.String(), "Attendo conferma del server") {
		t.Fatalf("avanzamento e conferma incoerenti: %s", progress.String())
	}
	saved, err := os.ReadFile(filepath.Join(state, "backups", receipt.ID+".backup"))
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatal("payload mismatch", err)
	}
	for _, method := range []string{"GET", "DELETE", "PUT", "PATCH"} {
		req, _ := http.NewRequest(method, server.URL+model.UploadPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Fatal(method, resp.StatusCode)
		}
	}
	if err = s.Revoke(key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Send(context.Background(), o, file, model.Metadata{Description: "revoked", OriginalName: "db.sql"}, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal("revoked key accepted", err)
	}
	if _, err = os.Stat(filepath.Join(state, "backups", receipt.ID+".backup")); err != nil {
		t.Fatal("revocation removed backup")
	}
}
func TestNoRedirectCredentials(t *testing.T) {
	c, err := client.HTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	if c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects enabled")
	}
}

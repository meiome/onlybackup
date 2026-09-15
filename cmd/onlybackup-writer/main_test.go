package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

const writerProcessEnv = "ONLYBACKUP_WRITER_TEST_PROCESS"

// TestWriterProcessHelper runs the real writer main loop in a child test
// process so TestWriterSIGTERMWaitsForAdmittedUpload can deliver a real signal.
func TestWriterProcessHelper(t *testing.T) {
	if os.Getenv(writerProcessEnv) != "1" {
		return
	}
	err := runArgs([]string{
		"--state", os.Getenv("ONLYBACKUP_WRITER_TEST_STATE"),
		"--socket", os.Getenv("ONLYBACKUP_WRITER_TEST_SOCKET"),
		"--reserve-free", "0",
		"--upload-timeout", "30s",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestWriterSIGTERMWaitsForAdmittedUpload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	profile := model.Profile{Name: "signal-test", TotalBytes: 2 << 20, MaxBackupBytes: 1 << 20, UploadsPerDay: 10, Concurrent: 1}
	if err = s.PutProfile(profile); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateKey("signal test", profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	socket := filepath.Join(root, "writer.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterProcessHelper$")
	var childLog bytes.Buffer
	cmd.Stderr = &childLog
	cmd.Env = append(os.Environ(),
		writerProcessEnv+"=1",
		"ONLYBACKUP_WRITER_TEST_STATE="+root,
		"ONLYBACKUP_WRITER_TEST_SOCKET="+socket,
	)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		exited <- cmd.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		select {
		case <-processDone:
		default:
			_ = cmd.Process.Kill()
			<-processDone
		}
	})

	waitForSocket(t, socket, exited, &childLog)
	data := bytes.Repeat([]byte("durable-sigterm-upload-"), 16<<10)
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	metadata, err := model.EncodeMetadata(model.Metadata{Description: "upload durante SIGTERM", OriginalName: "dump.bin"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	idempotencyKey, err := model.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	requestHeader := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: writer\r\nAuthorization: Bearer %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n%s: %s\r\n%s: %s\r\n%s: %s\r\nConnection: close\r\n\r\n", model.UploadPath, token, len(data), model.MetadataHeader, metadata, model.DigestHeader, digest, model.IdempotencyHeader, idempotencyKey)
	if _, err = io.WriteString(conn, requestHeader); err != nil {
		t.Fatal(err)
	}
	cut := len(data) / 3
	if _, err = conn.Write(data[:cut]); err != nil {
		t.Fatal(err)
	}
	waitForStaging(t, filepath.Join(root, "incoming"), exited, &childLog)

	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-exited:
		t.Fatalf("writer exited before admitted upload finished: %v\n%s", err, childLog.String())
	case <-time.After(150 * time.Millisecond):
	}
	if _, err = conn.Write(data[cut:]); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	var receipt model.Receipt
	if err = json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-exited:
		if err != nil {
			t.Fatalf("writer shutdown failed: %v\n%s", err, childLog.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not exit after admitted upload completed")
	}

	stored, err := os.ReadFile(filepath.Join(root, "backups", receipt.ID+".backup"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatal("positive receipt does not match durable backup")
	}
	s, err = store.Open(filepath.Join(root, "metadata.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup, err := s.Backup(receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Status != "complete" || backup.SHA256 != digest || backup.Size != int64(len(data)) {
		t.Fatal(backup)
	}
}

func waitForSocket(t *testing.T, socket string, exited <-chan error, childLog *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		select {
		case err := <-exited:
			t.Fatalf("writer exited before listening: %v\n%s", err, childLog.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("writer socket not ready\n%s", childLog.String())
}

func waitForStaging(t *testing.T, incoming string, exited <-chan error, childLog *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(incoming)
		if err == nil && len(entries) == 1 {
			info, statErr := entries[0].Info()
			if statErr == nil && info.Size() > 0 {
				return
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("writer exited before admitting upload: %v\n%s", err, childLog.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("writer did not stage upload\n%s", childLog.String())
}

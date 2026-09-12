package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestProtectedModeRejectsVaultSocketAliasBeforeTouchingState(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "vault")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(realDir, "vault.sock")
	if err := os.WriteFile(socket, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	err := runArgs([]string{"--state", filepath.Join(root, "missing"), "--socket", filepath.Join(alias, "vault.sock"), "--vault-socket", socket})
	if err == nil || !strings.Contains(err.Error(), "distinte") {
		t.Fatalf("expected alias rejection: %v", err)
	}
	got, err := os.ReadFile(socket)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("vault endpoint changed: %q %v", got, err)
	}
}

func TestProtectedModeCannotTakeOverLockedWriterSocket(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "writer.sock")
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.WriteFile(socket, []byte("active endpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	err = runArgs([]string{"--state", filepath.Join(root, "missing"), "--socket", socket, "--vault-socket", filepath.Join(root, "vault.sock")})
	if err == nil || !strings.Contains(err.Error(), "già attivo su questa socket") {
		t.Fatalf("expected socket lock rejection: %v", err)
	}
	got, err := os.ReadFile(socket)
	if err != nil || string(got) != "active endpoint" {
		t.Fatal("existing writer endpoint was changed")
	}
}

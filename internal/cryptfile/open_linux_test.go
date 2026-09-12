//go:build linux

package cryptfile

import (
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVerifyRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), path, 1, digest([]byte("x"))); err == nil {
		t.Fatal("FIFO archive accepted")
	}
}

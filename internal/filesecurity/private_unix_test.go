//go:build !windows

package filesecurity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateFileUnixPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivate(path, info); err == nil {
		t.Fatal("file permissivo accettato")
	}
	if err = RestrictPrivate(path); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivate(path, info); err != nil {
		t.Fatal(err)
	}
}

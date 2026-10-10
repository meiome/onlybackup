package maintenance

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
	"golang.org/x/sys/unix"
)

// Invoked only by scripts/systemd-layout-test.py inside a real systemd mount
// namespace. Other tests cover distinct service UIDs and the writer protocol.
func TestSystemdArchiveProbe(t *testing.T) {
	root := os.Getenv("ONLYBACKUP_SYSTEMD_STATE")
	role := os.Getenv("ONLYBACKUP_SYSTEMD_ROLE")
	if root == "" {
		t.Skip("requires disposable systemd fixture")
	}
	archive := filepath.Join(root, "archives", "backups", "00000000000000000000000000000001.backup")
	data := []byte("systemd mount regression")
	if role == "writer" {
		incoming := filepath.Join(root, "incoming", "upload")
		if err := os.WriteFile(incoming, data, 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(incoming, archive); err != nil {
			t.Fatalf("writer publication crossed mounts: %v", err)
		}
		if err := os.Remove(incoming); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, name := range []string{"metadata.db", "metadata.db-wal", "metadata.db-shm", "incoming/private"} {
		if f, err := os.Open(filepath.Join(root, name)); err == nil {
			f.Close()
			t.Fatalf("maintenance can read %s", name)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "unexpected"), data, 0600); err == nil {
		t.Fatal("maintenance can write state parent")
	}
	before, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	b := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000001", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}
	s := &Service{Root: root}
	if os.Getenv("ONLYBACKUP_SYSTEMD_SPLIT_MOUNTS") == "1" {
		err := s.execute(model.RetentionOperation{Kind: "quarantine", BackupID: b.ID}, b)
		if !errors.Is(err, unix.EXDEV) {
			t.Fatalf("old split mounts did not reproduce EXDEV: %v", err)
		}
		if _, err := os.Stat(archive); err != nil {
			t.Fatal("failed move lost source", err)
		}
		return
	}
	for _, kind := range []string{"quarantine", "recover", "quarantine", "purge"} {
		if err = s.execute(model.RetentionOperation{Kind: kind, BackupID: b.ID}, b); err != nil {
			t.Fatalf("%s under systemd: %v", kind, err)
		}
		if kind == "quarantine" {
			after, err := os.Stat(filepath.Join(root, "archives", "quarantine", b.ID+".backup"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("quarantine copied rather than renamed", err)
			}
		}
	}
	if _, err = os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("purge did not remove archive")
	}
}

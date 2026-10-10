package store

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func legacyArchive(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"backups", "quarantine"} {
		if err := os.Rename(filepath.Join(root, "archives", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, "archives")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestArchiveMigrationPreservesIdentityAndResumes(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-layout", true: "interrupted"}[partial], func(t *testing.T) {
			root := legacyArchive(t)
			catalog, err := os.ReadFile(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			identities := make(map[string]os.FileInfo)
			for _, name := range []string{"backups", "quarantine"} {
				path := filepath.Join(root, name, "preserved.backup")
				if err = os.WriteFile(path, []byte("immutable encrypted bytes"), 0440); err != nil {
					t.Fatal(err)
				}
				identities[name], err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = ValidateState(root); err == nil {
				t.Fatal("legacy layout accepted")
			}
			if partial {
				if err = os.Mkdir(filepath.Join(root, "archives"), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(filepath.Join(root, "backups"), filepath.Join(root, "archives", "backups")); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err = MigrateArchives(root); err != nil {
					t.Fatal(err)
				}
			}
			for name, before := range identities {
				after, statErr := os.Stat(filepath.Join(root, "archives", name, "preserved.backup"))
				if statErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("file identity/mode changed: %s %v", name, statErr)
				}
			}
			after, err := os.ReadFile(filepath.Join(root, "metadata.db"))
			if err != nil || !bytes.Equal(catalog, after) {
				t.Fatal("catalog rewritten", err)
			}
		})
	}
}

func TestArchiveMigrationRejectsCollisionAndSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "collision", true: "symlink"}[symlink], func(t *testing.T) {
			root := legacyArchive(t)
			if symlink {
				if err := os.Rename(filepath.Join(root, "quarantine"), filepath.Join(root, "saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "saved"), filepath.Join(root, "quarantine")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(root, "archives", "quarantine"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := MigrateArchives(root); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "backups")); err != nil {
				t.Fatal("moved first directory before checking second", err)
			}
		})
	}
}

func TestArchiveMigrationRejectsActiveExecutors(t *testing.T) {
	for _, name := range []string{"writer.lock", "maintenance"} {
		t.Run(name, func(t *testing.T) {
			root := legacyArchive(t)
			flags := unix.O_RDONLY | unix.O_DIRECTORY
			if name == "writer.lock" {
				flags = unix.O_CREAT | unix.O_RDWR
			}
			fd, err := unix.Open(filepath.Join(root, name), flags, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			if err = MigrateArchives(root); err == nil {
				t.Fatal("active executor migrated")
			}
			if _, err = os.Stat(filepath.Join(root, "archives")); !os.IsNotExist(err) {
				t.Fatal("migration changed layout before lock")
			}
		})
	}
}

func TestRejectedInitDoesNotObstructMigration(t *testing.T) {
	root := legacyArchive(t)
	if err := Init(root); err == nil {
		t.Fatal("reinitialized existing state")
	}
	if _, err := os.Stat(filepath.Join(root, "archives")); !os.IsNotExist(err) {
		t.Fatal("rejected init created migration destinations")
	}
	if err := MigrateArchives(root); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveMigrationFromPreMaintenanceLayout(t *testing.T) {
	root := legacyArchive(t)
	for _, name := range []string{"quarantine", "maintenance"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateArchives(root); err != nil {
		t.Fatal(err)
	}
	if err := ValidateState(root); err != nil {
		t.Fatal(err)
	}
}

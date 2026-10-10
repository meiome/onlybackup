package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// ValidateArchiveLayout refuses mixed layouts rather than silently abandoning
// files in the old locations. Migration is an explicit offline operation.
func ValidateArchiveLayout(root string) error {
	if err := rejectLegacyArchiveDirectories(root); err != nil {
		return err
	}
	return validateDirectoryModes(filepath.Join(root, "archives"), 0700, 0710)
}

func rejectLegacyArchiveDirectories(root string) error {
	for _, name := range []string{"backups", "quarantine"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return errors.New("layout archivio precedente: fermare tutti i servizi ed eseguire onlybackup-admin --state DIR migrate-archives")
		}
	}
	return nil
}

// MigrateArchives moves directory inodes, not backup bytes. Locks prevent an
// active writer or maintenance cycle from being migrated. The operator must
// also stop the receiver and service supervisors before invoking this command.
// A partially completed migration is safe to run again; collisions fail closed.
func MigrateArchives(root string) error {
	if err := validateDirectoryModes(root, 0700, 0710); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(root, "metadata.db")); err != nil || !info.Mode().IsRegular() {
		return errors.New("catalogo esistente richiesto; la migrazione non inizializza lo stato")
	}
	if err := validateDirectoryModes(filepath.Join(root, "incoming"), 0700); err != nil {
		return err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return err
	}
	rootStat := rootInfo.Sys().(*syscall.Stat_t)
	lockPaths := []string{filepath.Join(root, "writer.lock"), filepath.Join(root, "maintenance")}
	for i, path := range lockPaths {
		if i == 1 {
			// The writer lock is held before creating the executor directory
			// absent in pre-maintenance installations.
			if err := os.Mkdir(path, 0700); err == nil {
				if err = os.Chown(path, int(rootStat.Uid), int(rootStat.Gid)); err != nil {
					return err
				}
			} else if !os.IsExist(err) {
				return err
			}
			if err := validateDirectoryModes(path, 0700, 0770); err != nil {
				return err
			}
		}
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i == 0 {
			flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		fd, err := unix.Open(path, flags, 0600)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), path)
		defer f.Close()
		if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return fmt.Errorf("migrazione richiede writer e maintenance fermi: %w", err)
		}
	}
	info := rootInfo
	parent := filepath.Join(root, "archives")
	if err = os.Mkdir(parent, 0700); err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if err = os.Chown(parent, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
		if err = os.Chmod(parent, info.Mode().Perm()); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	if err = validateDirectoryModes(parent, 0700, 0710); err != nil {
		return err
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if parentInfo.Sys().(*syscall.Stat_t).Dev != rootStat.Dev {
		return errors.New("archives deve risiedere sullo stesso filesystem dello stato")
	}
	// Check both sides before moving either directory. Never merge directories,
	// follow symlinks, overwrite destinations, or copy across filesystems.
	var sources []string
	for _, name := range []string{"backups", "quarantine"} {
		old, dest := filepath.Join(root, name), filepath.Join(parent, name)
		_, oldErr := os.Lstat(old)
		_, newErr := os.Lstat(dest)
		if oldErr != nil && !os.IsNotExist(oldErr) {
			return oldErr
		}
		if newErr != nil && !os.IsNotExist(newErr) {
			return newErr
		}
		if name == "quarantine" && os.IsNotExist(oldErr) && os.IsNotExist(newErr) {
			if err = os.Mkdir(old, 0700); err != nil {
				return err
			}
			if err = os.Chown(old, int(rootStat.Uid), int(rootStat.Gid)); err != nil {
				return err
			}
			oldErr = nil
		}
		if (oldErr == nil) == (newErr == nil) {
			return fmt.Errorf("layout ambiguo o directory assente: %s", name)
		}
		path := dest
		if oldErr == nil {
			path = old
			sources = append(sources, name)
		}
		if err = validateDirectoryModes(path, 0700, 0770); err != nil {
			return err
		}
		pinfo, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if pinfo.Sys().(*syscall.Stat_t).Dev != info.Sys().(*syscall.Stat_t).Dev {
			return errors.New("migrazione richiede tutte le directory sullo stesso filesystem")
		}
	}
	for _, name := range sources {
		if err = unix.Renameat2(unix.AT_FDCWD, filepath.Join(root, name), unix.AT_FDCWD, filepath.Join(parent, name), unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		for _, path := range []string{parent, root} {
			f, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			syncErr := f.Sync()
			closeErr := f.Close()
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return ValidateState(root)
}

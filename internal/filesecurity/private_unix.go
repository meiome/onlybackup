//go:build !windows

package filesecurity

import (
	"errors"
	"os"
)

// CheckPrivate rejects private material readable or writable by group or other
// users. The caller is responsible for checking the expected file type.
func CheckPrivate(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("il file deve essere accessibile solo al proprietario (chmod 600)")
	}
	return nil
}

// RestrictPrivate applies the private-file policy used by CheckPrivate.
func RestrictPrivate(path string) error {
	return os.Chmod(path, 0600)
}

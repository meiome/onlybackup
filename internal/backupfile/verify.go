// Package backupfile verifies the immutable stored representation without any
// dependency on the catalog implementation.
package backupfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

func Verify(path string, size int64, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("dimensione o SHA-256 non corrispondenti")
	}
	return nil
}

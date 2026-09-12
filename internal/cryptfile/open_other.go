//go:build !linux

package cryptfile

import (
	"errors"
	"os"
)

// OpenRegular apre un file regolare esistente e rifiuta i link simbolici.
func OpenRegular(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("richiesto un file regolare non simbolico")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, errors.New("il file e cambiato durante l'apertura")
	}
	return f, nil
}

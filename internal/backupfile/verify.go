// Package backupfile verifies the immutable stored representation without any
// dependency on the catalog implementation.
package backupfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

func Verify(path string, size int64, digest string) error {
	return VerifyContext(context.Background(), path, size, digest)
}

// VerifyContext checks the complete digest while allowing shutdown, lease loss,
// and disconnected API requests to stop disk work between reads.
func VerifyContext(ctx context.Context, path string, size int64, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, contextReader{ctx, f}, make([]byte, 1<<20))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("dimensione o SHA-256 non corrispondenti")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

package backupfile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyContextIntegrityAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	data := make([]byte, 3<<20)
	data[len(data)-1] = 42
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := VerifyContext(context.Background(), path, int64(len(data)), digest); err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] = 43
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyContext(context.Background(), path, int64(len(data)), digest); err == nil {
		t.Fatal("tail corruption accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyContext(ctx, path, int64(len(data)), digest); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type cancellingReader struct{ cancel context.CancelFunc }

func (r cancellingReader) Read(p []byte) (int, error) { r.cancel(); p[0] = 1; return 1, nil }

func TestVerifyReaderStopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := contextReader{ctx, cancellingReader{cancel}}
	if _, err := r.Read(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 32)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

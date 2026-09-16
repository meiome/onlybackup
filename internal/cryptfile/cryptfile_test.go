package cryptfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/meiome/onlybackup/internal/filesecurity"
)

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encryptedFixture(t *testing.T, plaintext []byte) (cipherPath, identityPath string, ciphertext []byte) {
	t.Helper()
	dir := t.TempDir()
	identityPath = filepath.Join(dir, "identity.txt")
	recipient, err := GenerateIdentity(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.bin")
	writeFile(t, source, plaintext, 0600)
	prepared, err := EncryptToTemp(context.Background(), source, recipient, dir, int64(len(plaintext))+1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Remove()
	ciphertext, err = os.ReadFile(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	cipherPath = filepath.Join(dir, "archive.backup")
	writeFile(t, cipherPath, ciphertext, 0400)
	return cipherPath, identityPath, ciphertext
}

func TestEncryptRecoverRoundTripAndRawExport(t *testing.T) {
	plaintext := bytes.Repeat([]byte("backup-data-\x00\xff"), 600000)
	cipherPath, identityPath, ciphertext := encryptedFixture(t, plaintext)
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	dir := filepath.Dir(cipherPath)
	recovered := filepath.Join(dir, "recovered.bin")
	if err := Recover(context.Background(), cipherPath, recovered, int64(len(ciphertext)), digest(ciphertext), identityPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(recovered)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatal("decrypted output mismatch", err)
	}
	if info, err := os.Stat(recovered); err != nil {
		t.Fatalf("recovered stat: %v", err)
	} else if err = filesecurity.CheckPrivate(recovered, info); err != nil {
		t.Fatalf("recovered permissions: %v", err)
	}
	raw := filepath.Join(dir, "raw.age")
	if err = Recover(context.Background(), cipherPath, raw, int64(len(ciphertext)), digest(ciphertext), ""); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(raw)
	if err != nil || !bytes.Equal(got, ciphertext) {
		t.Fatal("raw export mismatch", err)
	}
}

func TestRecoveryRejectsWrongKeyTamperingAndTruncationWithoutOutput(t *testing.T) {
	plaintext := bytes.Repeat([]byte("authenticated data"), 10000)
	cipherPath, identityPath, ciphertext := encryptedFixture(t, plaintext)
	dir := filepath.Dir(cipherPath)
	wrongIdentity := filepath.Join(dir, "wrong-identity.txt")
	if _, err := GenerateIdentity(wrongIdentity); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		data     []byte
		identity string
	}{
		{name: "wrong-key", data: ciphertext, identity: wrongIdentity},
		{name: "tampered", data: append([]byte(nil), ciphertext...), identity: identityPath},
		{name: "truncated", data: ciphertext[:len(ciphertext)-1], identity: identityPath},
	}
	tests[1].data[len(tests[1].data)-1] ^= 0x80
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(dir, tc.name+".backup")
			writeFile(t, archive, tc.data, 0400)
			out := filepath.Join(dir, tc.name+".out")
			if err := Recover(context.Background(), archive, out, int64(len(tc.data)), digest(tc.data), tc.identity); err == nil {
				t.Fatal("recovery unexpectedly succeeded")
			}
			if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial output was published: %v", err)
			}
		})
	}
}

func TestRecoveryDoesNotOverwriteAndVerifiesStoredHash(t *testing.T) {
	plaintext := []byte("do not overwrite")
	cipherPath, identityPath, ciphertext := encryptedFixture(t, plaintext)
	dir := filepath.Dir(cipherPath)
	out := filepath.Join(dir, "existing")
	writeFile(t, out, []byte("keep"), 0600)
	if err := Recover(context.Background(), cipherPath, out, int64(len(ciphertext)), digest(ciphertext), identityPath); err == nil {
		t.Fatal("overwriting output was accepted")
	}
	if got, _ := os.ReadFile(out); string(got) != "keep" {
		t.Fatalf("existing output changed: %q", got)
	}
	missing := filepath.Join(dir, "missing")
	if err := Recover(context.Background(), cipherPath, missing, int64(len(ciphertext)), digest([]byte("different")), identityPath); err == nil {
		t.Fatal("incorrect archived hash was accepted")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output published after failed archive verification: %v", err)
	}
}

func TestEncryptionLimitDiskErrorsAndIdentityPermissions(t *testing.T) {
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity")
	recipient, err := GenerateIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = GenerateIdentity(identity); !errors.Is(err, os.ErrExist) {
		t.Fatalf("identity overwrite error: %v", err)
	}
	source := filepath.Join(dir, "source")
	writeFile(t, source, bytes.Repeat([]byte("x"), 1<<20), 0600)
	if _, err = EncryptToTemp(context.Background(), source, recipient, dir, 100); !errors.Is(err, ErrCiphertextLimit) {
		t.Fatalf("ciphertext cap error: %v", err)
	}
	if _, err = EncryptToTemp(context.Background(), source, recipient, filepath.Join(dir, "absent"), 2<<20); err == nil {
		t.Fatal("missing temporary directory accepted")
	}
	testInsecureIdentityPermissions(t, identity)
	link := filepath.Join(dir, "identity-link")
	if err = os.Symlink(identity, link); err != nil {
		t.Logf("test symlink non disponibile: %v", err)
	} else {
		if _, err = ReadIdentities(link); err == nil {
			t.Fatal("identity symlink accepted")
		}
		if err = Verify(context.Background(), link, 1, digest([]byte("x"))); err == nil {
			t.Fatal("archive symlink accepted")
		}
	}
	if err = Verify(context.Background(), source, maxArchivedBytes+1, digest([]byte("x"))); err == nil {
		t.Fatal("oversized expected archive accepted")
	}
}

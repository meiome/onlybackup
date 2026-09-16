// Package cryptfile implements the local age encryption and recovery file flows.
package cryptfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/meiome/onlybackup/internal/filesecurity"
)

const (
	// DefaultMaxCiphertextBytes bounds client-side temporary disk use unless the
	// operator selects a different limit.
	DefaultMaxCiphertextBytes int64 = 1 << 40
	maxIdentityBytes                = 64 << 10
	maxArchivedBytes          int64 = 1 << 60
)

var ErrCiphertextLimit = errors.New("il file cifrato supera il limite temporaneo configurato")

// Prepared is a rewindable encrypted temporary file ready for upload.
type Prepared struct {
	File   *os.File
	Path   string
	Size   int64
	SHA256 string
}

// Remove closes and removes a prepared ciphertext.
func (p *Prepared) Remove() {
	if p == nil {
		return
	}
	if p.File != nil {
		_ = p.File.Close()
	}
	if p.Path != "" {
		_ = os.Remove(p.Path)
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type cappedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrCiphertextLimit
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// EncryptToTemp encrypts a regular non-empty source to an age X25519 recipient.
// The returned file is positioned at offset zero. The caller must call Remove.
func EncryptToTemp(ctx context.Context, sourcePath, recipientText, tempDir string, maxBytes int64) (*Prepared, error) {
	if maxBytes <= 0 {
		return nil, errors.New("il limite del temporaneo cifrato deve essere positivo")
	}
	recipient, err := age.ParseX25519Recipient(strings.TrimSpace(recipientText))
	if err != nil {
		return nil, fmt.Errorf("destinatario age non valido: %w", err)
	}
	source, err := OpenRegular(sourcePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 {
		return nil, errors.New("richiesto un file regolare non vuoto; streaming non ancora disponibile")
	}

	temp, err := os.CreateTemp(tempDir, "onlybackup-encrypted-*")
	if err != nil {
		return nil, fmt.Errorf("creazione temporaneo cifrato: %w", err)
	}
	p := &Prepared{File: temp, Path: temp.Name()}
	ok := false
	defer func() {
		if !ok {
			p.Remove()
		}
	}()
	if err = temp.Chmod(0600); err != nil {
		return nil, fmt.Errorf("permessi temporaneo cifrato: %w", err)
	}
	hash := sha256.New()
	bounded := &cappedWriter{w: io.MultiWriter(temp, hash), remaining: maxBytes}
	encrypted, err := age.Encrypt(bounded, recipient)
	if err != nil {
		return nil, fmt.Errorf("inizializzazione cifratura age: %w", err)
	}
	_, copyErr := io.Copy(encrypted, &contextReader{ctx: ctx, r: source})
	closeErr := encrypted.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("cifratura age: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("finalizzazione cifratura age: %w", closeErr)
	}
	if err = temp.Sync(); err != nil {
		return nil, fmt.Errorf("sincronizzazione temporaneo cifrato: %w", err)
	}
	info, err = temp.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return nil, ErrCiphertextLimit
	}
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	p.Size = info.Size()
	p.SHA256 = hex.EncodeToString(hash.Sum(nil))
	ok = true
	return p, nil
}

// ReadIdentities reads a small private age identity file. It rejects symlinks,
// non-regular files and any access granted to group or other users.
func ReadIdentities(path string) ([]age.Identity, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, fmt.Errorf("file identita non sicuro: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err = filesecurity.CheckPrivate(path, info); err != nil {
		return nil, fmt.Errorf("file identita non sicuro: %w", err)
	}
	if info.Size() > maxIdentityBytes {
		return nil, errors.New("file identita troppo grande")
	}
	identities, err := age.ParseIdentities(io.LimitReader(f, maxIdentityBytes+1))
	if err != nil {
		return nil, fmt.Errorf("file identita age non valido: %w", err)
	}
	if len(identities) == 0 {
		return nil, errors.New("il file non contiene identita age")
	}
	return identities, nil
}

// Verify checks the exact size and SHA-256 of stored bytes without publishing
// an output file.
func Verify(ctx context.Context, sourcePath string, expectedSize int64, expectedSHA256 string) error {
	if err := validateExpected(expectedSize, expectedSHA256); err != nil {
		return err
	}
	f, err := OpenRegular(sourcePath)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, r: f}, expectedSize+1))
	if err != nil {
		return err
	}
	if n != expectedSize || hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return errors.New("verifica integrita del contenuto archiviato fallita")
	}
	return nil
}

// GenerateIdentity writes a new private X25519 age identity without replacing
// an existing path. The returned string is the corresponding public recipient.
func GenerateIdentity(path string) (recipient string, err error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		_ = f.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if err = filesecurity.RestrictPrivate(path); err != nil {
		return "", err
	}
	if _, err = fmt.Fprintf(f, "# created by onlybackup-recover\n%s\n", identity.String()); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	success = true
	return identity.Recipient().String(), nil
}

// Recover first copies and verifies the stored bytes into a private staging
// file in the output directory. It then either exports those raw bytes or
// decrypts them, and publishes the final output atomically without overwrite.
func Recover(ctx context.Context, sourcePath, outputPath string, expectedSize int64, expectedSHA256, identityFile string) error {
	if outputPath == "" {
		return errors.New("file di destinazione obbligatorio")
	}
	if err := validateExpected(expectedSize, expectedSHA256); err != nil {
		return err
	}
	var identities []age.Identity
	var err error
	if identityFile != "" {
		identities, err = ReadIdentities(identityFile)
		if err != nil {
			return err
		}
	}
	outDir := filepath.Dir(outputPath)
	verified, err := os.CreateTemp(outDir, ".onlybackup-verified-*")
	if err != nil {
		return err
	}
	verifiedPath := verified.Name()
	defer func() {
		_ = verified.Close()
		_ = os.Remove(verifiedPath)
	}()
	if err = filesecurity.RestrictPrivate(verifiedPath); err != nil {
		return err
	}
	source, err := OpenRegular(sourcePath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(verified, hash), io.LimitReader(&contextReader{ctx: ctx, r: source}, expectedSize+1))
	closeErr := source.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != expectedSize || hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return errors.New("verifica integrita del contenuto archiviato fallita")
	}
	if err = verified.Sync(); err != nil {
		return err
	}
	if _, err = verified.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if len(identities) == 0 {
		if err = verified.Close(); err != nil {
			return err
		}
		return publish(verifiedPath, outputPath)
	}

	plain, err := os.CreateTemp(outDir, ".onlybackup-recovered-*")
	if err != nil {
		return err
	}
	plainPath := plain.Name()
	defer func() {
		_ = plain.Close()
		_ = os.Remove(plainPath)
	}()
	if err = filesecurity.RestrictPrivate(plainPath); err != nil {
		return err
	}
	decrypted, err := age.Decrypt(verified, identities...)
	if err != nil {
		return fmt.Errorf("decifratura age: %w", err)
	}
	if _, err = io.Copy(plain, &contextReader{ctx: ctx, r: decrypted}); err != nil {
		return fmt.Errorf("decifratura age: %w", err)
	}
	if err = plain.Sync(); err != nil {
		return err
	}
	if err = plain.Close(); err != nil {
		return err
	}
	return publish(plainPath, outputPath)
}

func validateExpected(size int64, digest string) error {
	if size <= 0 || size > maxArchivedBytes {
		return errors.New("dimensione archiviata non valida")
	}
	b, err := hex.DecodeString(digest)
	if err != nil || len(b) != sha256.Size || digest != strings.ToLower(digest) {
		return errors.New("SHA-256 archiviato non valido")
	}
	return nil
}

func publish(stagedPath, outputPath string) error {
	if err := os.Link(stagedPath, outputPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("il file di destinazione esiste gia")
		}
		return fmt.Errorf("pubblicazione del recupero: %w", err)
	}
	if err := syncDir(filepath.Dir(outputPath)); err != nil {
		// The completed output is already visible. Do not remove it, because that
		// could destroy the only successfully recovered copy after a sync error.
		return fmt.Errorf("output pubblicato ma sincronizzazione directory fallita: %w", err)
	}
	return nil
}

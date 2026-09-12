// Package vault implements the private, deposit-only protocol between an
// untrusted writer and the authoritative archive process.
package vault

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/meiome/onlybackup/internal/model"
)

const (
	maxEnvelopeBytes = 16 << 10
	maxResponseBytes = 64 << 10
	maxUploadBytes   = int64(1 << 60)
)

type Envelope struct {
	Token          string         `json:"token"`
	Metadata       model.Metadata `json:"metadata"`
	Size           int64          `json:"size_bytes"`
	SHA256         string         `json:"sha256"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

type response struct {
	StatusCode int            `json:"status_code"`
	Receipt    *model.Receipt `json:"receipt,omitempty"`
	Error      string         `json:"error,omitempty"`
}

func writeFrame(w io.Writer, value any, limit uint32) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) == 0 || uint32(len(b)) > limit {
		return errors.New("frame troppo grande")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(b)))
	if err = writeAll(w, prefix[:]); err != nil {
		return err
	}
	return writeAll(w, b)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func readFrame(r io.Reader, value any, limit uint32) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return fmt.Errorf("prefisso frame: %w", err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > limit {
		return errors.New("lunghezza frame non consentita")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return fmt.Errorf("frame incompleto: %w", err)
	}
	if err := rejectDuplicateKeys(b); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("frame JSON non valido")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("dati aggiuntivi nel frame")
	}
	return nil
}

// rejectDuplicateKeys makes the strict decoder reject duplicate object fields,
// including fields nested in Metadata. encoding/json otherwise accepts the last
// occurrence, which makes security-sensitive envelopes ambiguous.
func rejectDuplicateKeys(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("chiave JSON non valida")
				}
				if _, exists := seen[key]; exists {
					return errors.New("campo JSON duplicato")
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		default:
			return errors.New("delimitatore JSON non valido")
		}
	}
	if err := walk(); err != nil {
		return errors.New("frame JSON non valido o ambiguo")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("dati aggiuntivi nel frame")
	}
	return nil
}

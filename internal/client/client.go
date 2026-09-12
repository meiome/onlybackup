// Package client sends ready-made files. It contains no read, list or delete API.
package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/cryptfile"
	"github.com/meiome/onlybackup/internal/model"
)

type Options struct {
	URL                     string `json:"url"`
	KeyFile                 string `json:"key_file"`
	CAFile                  string `json:"ca_file,omitempty"`
	EncryptTo               string `json:"encrypt_to,omitempty"`
	Plaintext               bool   `json:"plaintext,omitempty"`
	EncryptedTempLimitBytes int64  `json:"encrypted_temp_limit_bytes,omitempty"`
	EncryptedTempDir        string `json:"encrypted_temp_dir,omitempty"`
}

func ReadSecret(path string) (string, error) {
	f, err := cryptfile.OpenRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("il file della chiave deve essere regolare e accessibile solo al proprietario (chmod 600)")
	}
	b, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 256 || !model.ValidToken(token) {
		return "", errors.New("file della chiave non valido")
	}
	return token, nil
}
func HTTPClient(caFile string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("certificato CA non valido")
		}
		cfg.RootCAs = roots
	}
	tr := &http.Transport{TLSClientConfig: cfg, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 15 * time.Second, ExpectContinueTimeout: 2 * time.Second, ResponseHeaderTimeout: 2 * time.Hour, DisableCompression: true}
	return &http.Client{Transport: tr, Timeout: 2 * time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type progressReader struct {
	r           io.Reader
	total, n    int64
	start, last time.Time
	out         io.Writer
	complete    bool
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	now := time.Now()
	if p.out != nil && !p.complete && n > 0 && (now.Sub(p.last) > 500*time.Millisecond || p.n == p.total) {
		elapsed := now.Sub(p.start).Seconds()
		speed := float64(p.n) / elapsed
		remaining := time.Duration(0)
		if speed > 0 {
			remaining = time.Duration(float64(p.total-p.n)/speed) * time.Second
		}
		fmt.Fprintf(p.out, "\rInvio: %.1f%% | %d / %d byte | %.1f MiB/s | rimanente %s", 100*float64(p.n)/float64(p.total), p.n, p.total, speed/(1<<20), remaining)
		p.last = now
		if p.n == p.total {
			p.complete = true
			fmt.Fprint(p.out, "\nAttendo conferma del server...")
		}
	}
	return n, err
}

// Send hashes before transmission so changes or truncation cannot produce a successful receipt.
// A random operation key permits bounded retries without creating duplicate backups.
func Send(ctx context.Context, o Options, path string, m model.Metadata, progress io.Writer) (model.Receipt, error) {
	var receipt model.Receipt
	encrypted := o.EncryptTo != ""
	if encrypted && o.Plaintext {
		return receipt, errors.New("encrypt_to e plaintext non possono essere usati insieme")
	}
	if encrypted {
		if m.ContentFormat != "" && m.ContentFormat != model.ContentFormatAgeV1 {
			return receipt, errors.New("formato contenuto incompatibile con --encrypt-to")
		}
		if o.EncryptedTempLimitBytes < 0 {
			return receipt, errors.New("il limite del temporaneo cifrato non puo essere negativo")
		}
		m.ContentFormat = model.ContentFormatAgeV1
	} else {
		if !o.Plaintext {
			return receipt, errors.New("cifratura richiesta: configurare encrypt_to oppure scegliere esplicitamente plaintext")
		}
		if m.ContentFormat != "" {
			return receipt, errors.New("content_format richiede la cifratura configurata")
		}
		if o.EncryptedTempLimitBytes != 0 || o.EncryptedTempDir != "" {
			return receipt, errors.New("le opzioni del temporaneo cifrato richiedono encrypt_to")
		}
	}
	if err := m.Validate(); err != nil {
		return receipt, err
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return receipt, errors.New("URL richiesta: https://host:porta, senza credenziali o percorso")
	}
	token, err := ReadSecret(o.KeyFile)
	if err != nil {
		return receipt, err
	}
	httpClient, err := HTTPClient(o.CAFile)
	if err != nil {
		return receipt, err
	}
	defer httpClient.CloseIdleConnections()

	var f *os.File
	var size int64
	var digest string
	if encrypted {
		limit := o.EncryptedTempLimitBytes
		if limit == 0 {
			limit = cryptfile.DefaultMaxCiphertextBytes
		}
		if progress != nil {
			fmt.Fprintln(progress, "Cifratura age nel file temporaneo...")
		}
		prepared, prepareErr := cryptfile.EncryptToTemp(ctx, path, o.EncryptTo, o.EncryptedTempDir, limit)
		if prepareErr != nil {
			return receipt, prepareErr
		}
		defer prepared.Remove()
		f, size, digest = prepared.File, prepared.Size, prepared.SHA256
	} else {
		f, err = os.Open(path)
		if err != nil {
			return receipt, err
		}
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return receipt, statErr
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return receipt, errors.New("richiesto un file regolare non vuoto; streaming non ancora disponibile")
		}
		size = info.Size()
		if progress != nil {
			fmt.Fprintln(progress, "Calcolo SHA-256 del file...")
		}
		h := sha256.New()
		if _, err = io.Copy(h, &contextReader{ctx: ctx, r: f}); err != nil {
			return receipt, err
		}
		digest = hex.EncodeToString(h.Sum(nil))
		if _, err = f.Seek(0, 0); err != nil {
			return receipt, err
		}
	}
	if err = ctx.Err(); err != nil {
		return receipt, err
	}
	encoded, err := model.EncodeMetadata(m)
	if err != nil {
		return receipt, err
	}
	idempotencyKey, err := model.NewIdempotencyKey()
	if err != nil {
		return receipt, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt != 0 {
			if progress != nil {
				fmt.Fprintln(progress, "Ritento l'invio con la stessa chiave di idempotenza...")
			}
			delay := time.Duration(attempt*250) * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return receipt, ctx.Err()
			case <-timer.C:
			}
		}
		if _, err = f.Seek(0, 0); err != nil {
			return receipt, err
		}
		p := &progressReader{r: f, total: size, start: time.Now(), out: progress}
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(o.URL, "/")+model.UploadPath, p)
		if requestErr != nil {
			return receipt, requestErr
		}
		req.ContentLength = size
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set(model.MetadataHeader, encoded)
		req.Header.Set(model.DigestHeader, digest)
		req.Header.Set(model.IdempotencyHeader, idempotencyKey)
		req.Header.Set("Expect", "100-continue")
		resp, requestErr := httpClient.Do(req)
		if progress != nil {
			fmt.Fprintln(progress)
		}
		if requestErr != nil {
			if ctx.Err() != nil {
				return receipt, fmt.Errorf("esito non confermato: %w", ctx.Err())
			}
			lastErr = fmt.Errorf("esito non confermato: %w", requestErr)
			continue
		}
		if resp.StatusCode != http.StatusCreated {
			var body struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
			_ = resp.Body.Close()
			if body.Error == "" {
				body.Error = http.StatusText(resp.StatusCode)
			}
			lastErr = fmt.Errorf("invio non confermato (HTTP %d): %s", resp.StatusCode, body.Error)
			if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
				(resp.StatusCode == http.StatusConflict && body.Error == model.ErrIdempotencyInProgress.Error()) {
				continue
			}
			return receipt, lastErr
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&receipt)
		_ = resp.Body.Close()
		if decodeErr != nil {
			lastErr = errors.New("esito non confermato: ricevuta non valida")
			continue
		}
		if receipt.Status != "complete" || !model.ValidID(receipt.ID) || receipt.Size != size || receipt.SHA256 != digest {
			lastErr = errors.New("esito non confermato: ricevuta incoerente")
			continue
		}
		if _, err = time.Parse(time.RFC3339Nano, receipt.ReceivedAt); err != nil {
			lastErr = errors.New("esito non confermato: data ricevuta non valida")
			continue
		}
		if progress != nil {
			fmt.Fprintln(progress, "Backup ricevuto e salvato.")
		}
		return receipt, nil
	}
	return receipt, fmt.Errorf("%w; tentativi automatici esauriti", lastErr)
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

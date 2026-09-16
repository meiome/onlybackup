package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/meiome/onlybackup/internal/client"
	"github.com/meiome/onlybackup/internal/filesecurity"
	"github.com/meiome/onlybackup/internal/model"
)

func writeReceipt(path string, receipt model.Receipt) (err error) {
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err = filesecurity.RestrictPrivate(path); err != nil {
		return err
	}
	if _, err = file.Write(encoded); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}

func run() error {
	if len(os.Args) < 2 || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Fprintln(os.Stderr, "Uso: onlybackup send [--config client.json] [--url https://host:8443 --key-file chiave] [--encrypt-to age1... | --plaintext] [--encrypted-temp-limit DIMENSIONE --encrypted-temp-dir DIR] --description TESTO [--name NOME] [--receipt FILE_NUOVO] FILE\nLa cifratura richiede un destinatario age. L'invio in chiaro richiede --plaintext esplicito. Solo file gia pronti. Nessuna operazione di download, elenco o cancellazione.")
		return nil
	}
	if os.Args[1] != "send" {
		return errors.New("unica operazione disponibile: send")
	}
	f := flag.NewFlagSet("send", flag.ContinueOnError)
	config := f.String("config", "", "configurazione JSON con url, key_file e ca_file opzionale")
	url := f.String("url", "", "URL HTTPS del server")
	key := f.String("key-file", "", "file della chiave, permessi 0600")
	ca := f.String("ca-file", "", "CA privata opzionale")
	encryptTo := f.String("encrypt-to", "", "destinatario pubblico age X25519; abilita la cifratura")
	plaintext := f.Bool("plaintext", false, "consenti esplicitamente l'invio in chiaro")
	encryptedTempLimit := f.String("encrypted-temp-limit", "", "limite del temporaneo cifrato (default 1TiB)")
	encryptedTempDir := f.String("encrypted-temp-dir", "", "directory del temporaneo cifrato")
	description := f.String("description", "", "descrizione obbligatoria, massimo 1000 caratteri")
	label := f.String("label", "", "alias di --description")
	name := f.String("name", "", "nome originale, ricavato dal file se omesso")
	receiptPath := f.String("receipt", "", "salva la ricevuta in un file nuovo senza sovrascrivere")
	quiet := f.Bool("quiet", false, "non mostrare avanzamento su stderr")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("specificare un file dopo le opzioni; --stdin non ancora disponibile")
	}
	var o client.Options
	if *config != "" {
		h, err := os.Open(*config)
		if err != nil {
			return err
		}
		defer h.Close()
		d := json.NewDecoder(io.LimitReader(h, 64<<10))
		d.DisallowUnknownFields()
		if err = d.Decode(&o); err != nil {
			return err
		}
		var extra any
		if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
			return errors.New("configurazione con dati aggiuntivi")
		}
		for _, p := range []*string{&o.KeyFile, &o.CAFile, &o.EncryptedTempDir} {
			if *p != "" && !filepath.IsAbs(*p) {
				*p = filepath.Join(filepath.Dir(*config), *p)
			}
		}
	}
	if *url != "" {
		o.URL = *url
	}
	if *key != "" {
		o.KeyFile = *key
	}
	if *ca != "" {
		o.CAFile = *ca
	}
	if *encryptTo != "" && *plaintext {
		return errors.New("usare --encrypt-to oppure --plaintext")
	}
	if *encryptTo != "" {
		o.EncryptTo = *encryptTo
		o.Plaintext = false
	}
	if *plaintext {
		o.Plaintext = true
		o.EncryptTo = ""
	}
	if *encryptedTempDir != "" {
		o.EncryptedTempDir = *encryptedTempDir
	}
	if *encryptedTempLimit != "" {
		n, err := model.ParseBytes(*encryptedTempLimit)
		if err != nil || n <= 0 {
			return errors.New("--encrypted-temp-limit non valido: usare una dimensione positiva, per esempio 20GiB")
		}
		o.EncryptedTempLimitBytes = n
	}
	if *description != "" && *label != "" {
		return errors.New("usare --description oppure --label")
	}
	if *description == "" {
		*description = *label
	}
	if *name == "" {
		*name = filepath.Base(f.Arg(0))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var progress io.Writer = os.Stderr
	if *quiet {
		progress = nil
	}
	receipt, err := client.Send(ctx, o, f.Arg(0), model.Metadata{Description: *description, OriginalName: *name}, progress)
	if err != nil {
		return err
	}
	if *receiptPath != "" {
		if err = writeReceipt(*receiptPath, receipt); err != nil {
			return fmt.Errorf("salvataggio ricevuta: %w", err)
		}
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(receipt)
}
func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

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
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/cryptfile"
	"github.com/meiome/onlybackup/internal/model"
)

type recoveryTarget struct {
	ID            string
	Source        string
	Size          int64
	SHA256        string
	ContentFormat string
	OriginalName  string
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func targetFromReceipt(receiptPath, archivePath string) (recoveryTarget, error) {
	var target recoveryTarget
	receiptFile, err := cryptfile.OpenRegular(receiptPath)
	if err != nil {
		return target, fmt.Errorf("ricevuta non valida: %w", err)
	}
	defer receiptFile.Close()
	info, err := receiptFile.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 64<<10 {
		return target, errors.New("dimensione della ricevuta non valida")
	}
	decoder := json.NewDecoder(receiptFile)
	decoder.DisallowUnknownFields()
	var receipt model.Receipt
	if err = decoder.Decode(&receipt); err != nil {
		return target, fmt.Errorf("ricevuta JSON non valida: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return target, errors.New("ricevuta con dati aggiuntivi")
	}
	if !model.ValidID(receipt.ID) || receipt.Status != "complete" || receipt.Size <= 0 || !model.ValidDigest(receipt.SHA256) {
		return target, errors.New("ricevuta incompleta o non valida")
	}
	if _, err = time.Parse(time.RFC3339Nano, receipt.ReceivedAt); err != nil {
		return target, errors.New("data della ricevuta non valida")
	}
	return recoveryTarget{ID: receipt.ID, Source: archivePath, Size: receipt.Size, SHA256: receipt.SHA256}, nil
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "keygen" {
		f := flag.NewFlagSet("keygen", flag.ContinueOnError)
		out := f.String("out", "", "nuovo file privato age, creato con permessi 0600")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *out == "" {
			return errors.New("uso: onlybackup-recover keygen --out FILE_IDENTITA")
		}
		recipient, err := cryptfile.GenerateIdentity(*out)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"recipient": recipient})
	}
	if len(args) > 0 && args[0] == "recover" {
		args = args[1:]
	}
	f := flag.NewFlagSet("recover", flag.ContinueOnError)
	root := f.String("state", "./var/onlybackup", "directory locale archivio; non disponibile nel client Windows")
	id := f.String("id", "", "identificativo backup")
	receipt := f.String("receipt", "", "ricevuta JSON per recupero diretto senza catalogo")
	archive := f.String("archive", "", "file .backup associato alla ricevuta")
	out := f.String("out", "", "file nuovo di destinazione; se omesso verifica soltanto")
	identityFile := f.String("identity-file", "", "file privato age con permessi o ACL riservati")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	direct := *receipt != "" || *archive != ""
	var target recoveryTarget
	var err error
	if direct {
		if *receipt == "" || *archive == "" {
			return errors.New("usare insieme --receipt e --archive")
		}
		target, err = targetFromReceipt(*receipt, *archive)
		if err == nil && *id != "" && *id != target.ID {
			err = errors.New("ID richiesto diverso dalla ricevuta")
		}
	} else {
		if !model.ValidID(*id) {
			return errors.New("ID backup non valido")
		}
		target, err = targetFromState(*root, *id)
	}
	if err != nil {
		return err
	}
	if *identityFile != "" && target.ContentFormat != "" && target.ContentFormat != model.ContentFormatAgeV1 {
		return errors.New("--identity-file richiede un backup con content_format age-v1")
	}
	if *identityFile != "" && *out == "" {
		return errors.New("--identity-file richiede --out")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *out == "" {
		if err = cryptfile.Verify(ctx, target.Source, target.Size, target.SHA256); err != nil {
			return err
		}
	} else if err = cryptfile.Recover(ctx, target.Source, *out, target.Size, target.SHA256, *identityFile); err != nil {
		return err
	}
	contentFormat := target.ContentFormat
	if direct && *identityFile != "" {
		contentFormat = model.ContentFormatAgeV1
	}
	return printJSON(map[string]any{"id": target.ID, "verified": true, "content_format": contentFormat, "decrypted": *identityFile != "", "original_name": target.OriginalName, "output": *out})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

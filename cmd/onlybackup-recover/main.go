package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/meiome/onlybackup/internal/cryptfile"
	"github.com/meiome/onlybackup/internal/localcli"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

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
		return localcli.Print(os.Stdout, map[string]any{"recipient": recipient})
	}
	if len(args) > 0 && args[0] == "recover" {
		args = args[1:]
	}
	f := flag.NewFlagSet("recover", flag.ContinueOnError)
	root := f.String("state", "./var/onlybackup", "directory locale archivio")
	id := f.String("id", "", "identificativo backup")
	out := f.String("out", "", "file nuovo di destinazione; se omesso verifica soltanto")
	identityFile := f.String("identity-file", "", "file privato age 0600 per decifrare esplicitamente")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	if !model.ValidID(*id) {
		return errors.New("ID backup non valido")
	}
	s, err := store.Open(filepath.Join(*root, "metadata.db"), true)
	if err != nil {
		return err
	}
	defer s.Close()
	b, err := s.Backup(*id)
	if err != nil {
		return err
	}
	if b.Status != "complete" {
		return errors.New("backup non completato")
	}
	src := filepath.Join(*root, "backups", b.ID+".backup")
	if *identityFile != "" && b.ContentFormat != model.ContentFormatAgeV1 {
		return errors.New("--identity-file richiede un backup con content_format age-v1")
	}
	if *identityFile != "" && *out == "" {
		return errors.New("--identity-file richiede --out")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *out == "" {
		if err = cryptfile.Verify(ctx, src, b.Size, b.SHA256); err != nil {
			return err
		}
	} else if err = cryptfile.Recover(ctx, src, *out, b.Size, b.SHA256, *identityFile); err != nil {
		return err
	}
	return localcli.Print(os.Stdout, map[string]any{"id": b.ID, "verified": true, "content_format": b.ContentFormat, "decrypted": *identityFile != "", "original_name": b.OriginalName, "output": *out})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

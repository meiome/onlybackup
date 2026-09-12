package localcli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

func Print(out io.Writer, v any) error {
	e := json.NewEncoder(out)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func flags(name string, errOut io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(errOut)
	return f
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("argomenti inattesi: %v", f.Args())
	}
	return nil
}

func Admin(args []string, out, errOut io.Writer) error {
	global := flags("onlybackup-admin", errOut)
	root := global.String("state", "./var/onlybackup", "directory locale dell'archivio")
	global.Usage = func() {
		fmt.Fprintln(errOut, "Uso: onlybackup-admin [--state DIR] init | profiles list/set | keys create/list/revoke/assign | backups list/status | quota\nTutte le operazioni sono locali. I comandi di consultazione restituiscono JSON.")
	}
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		global.Usage()
		return flag.ErrHelp
	}
	if args[0] == "init" {
		if len(args) != 1 {
			return errors.New("init non accetta altri argomenti")
		}
		if err := store.Init(*root); err != nil {
			return err
		}
		return Print(out, map[string]string{"state": *root, "status": "initialized"})
	}
	readOnly := args[0] == "quota" || len(args) > 1 && (args[1] == "list" || args[0] == "backups")
	s, err := store.Open(filepath.Join(*root, "metadata.db"), readOnly)
	if err != nil {
		return err
	}
	defer s.Close()
	switch args[0] {
	case "profiles":
		if len(args) < 2 {
			return errors.New("profiles richiede list o set")
		}
		switch args[1] {
		case "list":
			if len(args) != 2 {
				return errors.New("profiles list non accetta argomenti")
			}
			p, err := s.Profiles()
			if err != nil {
				return err
			}
			return Print(out, p)
		case "set":
			f := flags("profiles set", errOut)
			name := f.String("name", "", "nome libero del profilo")
			total := f.String("total", "", "spazio totale, es. 500GiB")
			single := f.String("max-backup", "", "dimensione massima, es. 20GiB")
			daily := f.Int64("daily", 24, "invii ammessi nelle ultime 24 ore")
			concurrent := f.Int64("concurrent", 2, "upload simultanei")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			t, err := model.ParseBytes(*total)
			if err != nil {
				return err
			}
			m, err := model.ParseBytes(*single)
			if err != nil {
				return err
			}
			p := model.Profile{Name: *name, TotalBytes: t, MaxBackupBytes: m, UploadsPerDay: *daily, Concurrent: *concurrent}
			if err = s.PutProfile(p); err != nil {
				return err
			}
			return Print(out, p)
		}
		return errors.New("comando profiles sconosciuto")
	case "keys":
		if len(args) < 2 {
			return errors.New("keys richiede create, list, revoke o assign")
		}
		switch args[1] {
		case "list":
			if len(args) != 2 {
				return errors.New("keys list non accetta argomenti")
			}
			k, err := s.Keys()
			if err != nil {
				return err
			}
			return Print(out, k)
		case "create":
			f := flags("keys create", errOut)
			name := f.String("name", "", "nome della chiave, senza vincolo alla macchina")
			profile := f.String("profile", "", "profilo assegnato")
			dest := f.String("out", "", "file nuovo in cui scrivere la chiave (0600)")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			if *dest == "" {
				return errors.New("--out obbligatorio; la chiave non viene stampata")
			}
			secret, err := os.OpenFile(*dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			saved := false
			defer func() {
				secret.Close()
				if !saved {
					os.Remove(*dest)
				}
			}()
			k, token, err := s.CreateKey(*name, *profile)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintln(secret, token); err == nil {
				err = secret.Sync()
			}
			if closeErr := secret.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				s.Revoke(k.ID)
				return err
			}
			saved = true
			return Print(out, k)
		case "revoke":
			f := flags("keys revoke", errOut)
			id := f.String("id", "", "ID della chiave")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			if err = s.Revoke(*id); err != nil {
				return err
			}
			return Print(out, map[string]string{"id": *id, "status": "revoked"})
		case "assign":
			f := flags("keys assign", errOut)
			id := f.String("id", "", "ID della chiave")
			p := f.String("profile", "", "nuovo profilo")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			if err = s.Assign(*id, *p); err != nil {
				return err
			}
			return Print(out, map[string]string{"id": *id, "profile": *p})
		}
		return errors.New("comando keys sconosciuto")
	case "backups":
		if len(args) < 2 {
			return errors.New("backups richiede list o status")
		}
		switch args[1] {
		case "list":
			f := flags("backups list", errOut)
			key := f.String("key", "", "filtra per ID chiave")
			limit := f.Int("limit", 100, "numero massimo di risultati, fino a 10000")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			b, err := s.Backups(*key, *limit)
			if err != nil {
				return err
			}
			return Print(out, b)
		case "status":
			f := flags("backups status", errOut)
			id := f.String("id", "", "ID backup")
			if err := parse(f, args[2:]); err != nil {
				return err
			}
			b, err := s.Backup(*id)
			if err != nil {
				return err
			}
			return Print(out, b)
		}
		return errors.New("comando backups sconosciuto")
	case "quota":
		f := flags("quota", errOut)
		id := f.String("key", "", "ID della chiave")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		q, err := s.Quota(*id, time.Now())
		if err != nil {
			return err
		}
		return Print(out, q)
	}
	return errors.New("comando sconosciuto")
}

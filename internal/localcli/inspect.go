package localcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/meiome/onlybackup/internal/store"
)

// Inspect opens SQLite in read-only mode and exposes metadata only.
func Inspect(args []string, out, errOut io.Writer) error {
	f := flags("onlybackup-inspect", errOut)
	root := f.String("state", "./var/onlybackup", "directory locale archivio")
	f.Usage = func() {
		fmt.Fprintln(errOut, "Uso: onlybackup-inspect [--state DIR] list [--key ID --limit N] | status --id ID | quota --key ID\nSola lettura dei metadati, output JSON.")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	args = f.Args()
	if len(args) == 0 {
		f.Usage()
		return flag.ErrHelp
	}
	s, err := store.Open(filepath.Join(*root, "metadata.db"), true)
	if err != nil {
		return err
	}
	defer s.Close()
	switch args[0] {
	case "list":
		f := flags("list", errOut)
		key := f.String("key", "", "ID chiave opzionale")
		limit := f.Int("limit", 100, "numero di risultati")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		b, err := s.Backups(*key, *limit)
		if err != nil {
			return err
		}
		return Print(out, b)
	case "status":
		f := flags("status", errOut)
		id := f.String("id", "", "ID backup")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		b, err := s.Backup(*id)
		if err != nil {
			return err
		}
		return Print(out, b)
	case "quota":
		f := flags("quota", errOut)
		key := f.String("key", "", "ID chiave")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		q, err := s.Quota(*key, time.Now())
		if err != nil {
			return err
		}
		return Print(out, q)
	}
	return errors.New("operazione non disponibile: sono ammessi solo list, status e quota")
}

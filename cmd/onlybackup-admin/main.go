package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/meiome/onlybackup/internal/localcli"
	"os"
)

func main() {
	if err := localcli.Admin(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

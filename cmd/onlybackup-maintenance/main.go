package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/maintenance"
)

func run(args []string) error {
	flags := flag.NewFlagSet("onlybackup-maintenance", flag.ContinueOnError)
	root := flags.String("state", "./var/onlybackup", "directory archivio")
	socket := flags.String("socket", "", "socket Unix manutenzione (default: STATE/maintenance.sock)")
	interval := flags.Duration("interval", maintenance.DefaultCycleInterval, "intervallo controlli")
	scrub := flags.Duration("scrub-period", maintenance.DefaultScrubPeriod, "eta massima della verifica SHA-256 completa di ogni file archiviato")
	credentials := flags.String("mail-credentials", "", "file JSON 0600 con username/password SMTP")
	once := flags.Bool("once", false, "esegue un solo ciclo (test/amministrazione)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	if *interval < time.Minute {
		return errors.New("intervallo minimo un minuto")
	}
	if *scrub < time.Hour {
		return errors.New("scrub-period minimo un'ora")
	}
	if *socket == "" {
		*socket = filepath.Join(*root, "maintenance.sock")
	}
	hostname, _ := os.Hostname()
	owner := fmt.Sprintf("%s:%d", hostname, os.Getpid())
	service := maintenance.New(*root, *socket, owner, maintenance.SMTP{CredentialsPath: *credentials, Timeout: 30 * time.Second})
	service.ScrubPeriod = *scrub
	service.CycleInterval = *interval
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := service.RunOnce(ctx); err != nil {
		if maintenance.WriterUnavailable(err) {
			_ = service.NotifyWriterFailure(ctx, err)
		}
		if *once {
			return err
		}
		log.Printf("ciclo manutenzione: %v", err)
	} else if *once {
		return nil
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := service.RunOnce(ctx); err != nil {
				if maintenance.WriterUnavailable(err) {
					_ = service.NotifyWriterFailure(ctx, err)
				}
				log.Printf("ciclo manutenzione: %v", err)
			}
		}
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/ingest"
	"github.com/meiome/onlybackup/internal/lifecycle"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

func run() error {
	return runArgs(os.Args[1:])
}

func runArgs(args []string) error {
	flags := flag.NewFlagSet("onlybackup-writer", flag.ContinueOnError)
	root := flags.String("state", "./var/onlybackup", "directory archivio inizializzato")
	socket := flags.String("socket", "./var/onlybackup/writer.sock", "socket Unix locale")
	reserve := flags.String("reserve-free", "1GiB", "spazio libero minimo sul filesystem")
	timeout := flags.Duration("upload-timeout", 2*time.Hour, "durata massima richiesta")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	if *timeout < time.Second {
		return errors.New("timeout troppo breve")
	}
	free, err := model.ParseBytes(*reserve)
	if err != nil {
		return err
	}
	if err = store.ValidateState(*root); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(*root, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("un writer è già attivo su questo archivio")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := store.Open(filepath.Join(*root, "metadata.db"), false)
	if err != nil {
		return err
	}
	defer s.Close()
	writer := ingest.New(s, *root, free)
	if err = writer.Reconcile(); err != nil {
		return err
	}
	// A separate lock protects the listening socket even when no archive is open.
	socketLock, err := os.OpenFile(*socket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer socketLock.Close()
	if err = syscall.Flock(int(socketLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("un writer è già attivo su questa socket")
	}
	defer syscall.Flock(int(socketLock.Fd()), syscall.LOCK_UN)
	if info, err := os.Lstat(*socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("il percorso socket esistente non è una socket")
		}
		if err = os.Remove(*socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(*socket)
	if err = os.Chmod(*socket, 0660); err != nil {
		return err
	}
	tracker := lifecycle.NewTracker()
	server := &http.Server{Handler: tracker.Handler(writer), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: *timeout, WriteTimeout: *timeout + time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: model.MaxHeaderBytes}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("writer in ascolto sulla socket %s", *socket)
	forced, err := lifecycle.Serve(ctx, server, tracker, 15*time.Second, func() error { return server.Serve(listener) })
	if forced {
		log.Printf("writer: tempo di arresto scaduto; connessioni attive interrotte")
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

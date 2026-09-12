package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
	"github.com/meiome/onlybackup/internal/vault"
)

func run() error {
	state := flag.String("state", "./var/onlybackup", "directory dell'archivio autorevole inizializzato")
	socket := flag.String("socket", "./var/onlybackup/vault.sock", "socket Unix privata di deposito")
	reserve := flag.String("reserve-free", "1GiB", "spazio libero minimo sul filesystem")
	uploadTimeout := flag.Duration("upload-timeout", 2*time.Hour, "durata massima di un deposito")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	if *uploadTimeout < time.Second {
		return errors.New("timeout troppo breve")
	}
	reserveFree, err := model.ParseBytes(*reserve)
	if err != nil {
		return err
	}

	lock, err := os.OpenFile(filepath.Join(*state, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("un writer o vault è già attivo su questo archivio")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	socketLock, err := os.OpenFile(*socket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer socketLock.Close()
	if err = syscall.Flock(int(socketLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("un vault è già attivo su questa socket")
	}
	defer syscall.Flock(int(socketLock.Fd()), syscall.LOCK_UN)

	catalogue, err := store.Open(filepath.Join(*state, "metadata.db"), false)
	if err != nil {
		return err
	}
	defer catalogue.Close()
	server := vault.NewServer(catalogue, *state, reserveFree, *uploadTimeout)
	if err = server.Reconcile(); err != nil {
		return err
	}

	if info, statErr := os.Lstat(*socket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("il percorso socket esistente non è una socket")
		}
		probe, dialErr := net.DialTimeout("unix", *socket, time.Second)
		if dialErr == nil {
			_ = probe.Close()
			return errors.New("una socket vault è già attiva su questo percorso")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return fmt.Errorf("impossibile verificare la socket vault esistente: %w", dialErr)
		}
		if err = os.Remove(*socket); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	log.Printf("vault in ascolto sulla socket %s", *socket)
	serveReturned := false
	select {
	case err = <-serveDone:
		serveReturned = true
	case <-ctx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	shutdownErr := server.Shutdown(shutdownCtx)
	shutdownCancel()
	if !serveReturned {
		err = errors.Join(err, <-serveDone)
	}
	return errors.Join(err, shutdownErr)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

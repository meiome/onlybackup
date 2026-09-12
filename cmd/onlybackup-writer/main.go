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
	"github.com/meiome/onlybackup/internal/vault"
)

func run() error {
	return runArgs(os.Args[1:])
}

func runArgs(args []string) error {
	flags := flag.NewFlagSet("onlybackup-writer", flag.ContinueOnError)
	root := flags.String("state", "./var/onlybackup", "directory archivio inizializzato")
	socket := flags.String("socket", "./var/onlybackup/writer.sock", "socket Unix locale")
	vaultSocket := flags.String("vault-socket", "", "socket dell'archiviatore separato; abilita deposito senza accesso locale all'archivio")
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
	if *vaultSocket != "" {
		listenPath, err := filepath.Abs(*socket)
		if err != nil {
			return err
		}
		vaultPath, err := filepath.Abs(*vaultSocket)
		if err != nil {
			return err
		}
		// Resolve existing parent directories too, so a directory symlink cannot
		// alias the private vault endpoint during a same-user local deployment.
		if parent, e := filepath.EvalSymlinks(filepath.Dir(listenPath)); e == nil {
			listenPath = filepath.Join(parent, filepath.Base(listenPath))
		}
		if parent, e := filepath.EvalSymlinks(filepath.Dir(vaultPath)); e == nil {
			vaultPath = filepath.Join(parent, filepath.Base(vaultPath))
		}
		if listenPath == vaultPath {
			return errors.New("socket writer e vault devono essere distinte")
		}
	}
	free, err := model.ParseBytes(*reserve)
	if err != nil {
		return err
	}
	var writer http.Handler
	if *vaultSocket != "" {
		writer = vault.NewGateway(*vaultSocket, *timeout)
	} else {
		lock, err := os.OpenFile(filepath.Join(*root, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return errors.New("un writer o archiviatore è già attivo su questo archivio")
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		s, err := store.Open(filepath.Join(*root, "metadata.db"), false)
		if err != nil {
			return err
		}
		defer s.Close()
		local := ingest.New(s, *root, free)
		if err = local.Reconcile(); err != nil {
			return err
		}
		writer = local
		log.Printf("writer in modalità locale: archivio non isolato dal processo writer")
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

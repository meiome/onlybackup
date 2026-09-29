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
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/ingest"
	"github.com/meiome/onlybackup/internal/lifecycle"
	"github.com/meiome/onlybackup/internal/localapi"
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
	adminSocket := flags.String("admin-socket", "", "socket Unix amministrativa (default: STATE/admin.sock)")
	maintenanceSocket := flags.String("maintenance-socket", "", "socket Unix manutenzione (default: STATE/maintenance.sock)")
	adminUser := flags.String("admin-user", "", "utente Unix autorizzato alla socket amministrativa (default: UID corrente)")
	maintenanceUser := flags.String("maintenance-user", "", "utente Unix autorizzato alla manutenzione (default: UID corrente)")
	adminGroup := flags.String("admin-group", "", "gruppo proprietario della socket amministrativa")
	maintenanceGroup := flags.String("maintenance-group", "", "gruppo proprietario della socket manutenzione")
	ingestGroup := flags.String("ingest-group", "", "gruppo proprietario della socket di deposito")
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
	if *adminSocket == "" {
		*adminSocket = filepath.Join(*root, "admin.sock")
	}
	if *maintenanceSocket == "" {
		*maintenanceSocket = filepath.Join(*root, "maintenance.sock")
	}
	if *socket == *adminSocket || *socket == *maintenanceSocket || *adminSocket == *maintenanceSocket {
		return errors.New("le socket deposito, amministrazione e manutenzione devono essere distinte")
	}
	adminUID, err := resolveUID(*adminUser)
	if err != nil {
		return err
	}
	maintenanceUID, err := resolveUID(*maintenanceUser)
	if err != nil {
		return err
	}
	free, err := model.ParseBytes(*reserve)
	if err != nil {
		return err
	}
	if err = store.PrepareMaintenanceState(*root); err != nil {
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
	if err = s.SetReserveFree(free); err != nil {
		return err
	}
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
	if err = setSocketGroup(*socket, *ingestGroup); err != nil {
		return err
	}
	adminListener, err := listenPrivileged(*adminSocket, *adminGroup, func(peer localapi.Peer) bool { return peer.UID == adminUID })
	if err != nil {
		return err
	}
	defer adminListener.Close()
	defer os.Remove(*adminSocket)
	maintenanceListener, err := listenPrivileged(*maintenanceSocket, *maintenanceGroup, func(peer localapi.Peer) bool { return peer.UID == maintenanceUID })
	if err != nil {
		return err
	}
	defer maintenanceListener.Close()
	defer os.Remove(*maintenanceSocket)
	tracker := lifecycle.NewTracker()
	server := &http.Server{Handler: tracker.Handler(writer), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: *timeout, WriteTimeout: *timeout + time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: model.MaxHeaderBytes}
	api := localapi.New(s, *root)
	adminServer := &http.Server{Handler: tracker.Handler(api.AdminHandler()), ConnContext: localapi.ConnContext, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	maintenanceServer := &http.Server{Handler: tracker.Handler(api.MaintenanceHandler()), ConnContext: localapi.ConnContext, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	localErrors := make(chan error, 2)
	var localServers sync.WaitGroup
	serveLocal := func(localServer *http.Server, localListener net.Listener) {
		defer localServers.Done()
		if serveErr := localServer.Serve(localListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			localErrors <- serveErr
			cancel()
		}
	}
	localServers.Add(2)
	go serveLocal(adminServer, adminListener)
	go serveLocal(maintenanceServer, maintenanceListener)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		_ = adminServer.Shutdown(shutdownCtx)
		_ = maintenanceServer.Shutdown(shutdownCtx)
	}()
	log.Printf("writer in ascolto: deposito=%s admin=%s manutenzione=%s", *socket, *adminSocket, *maintenanceSocket)
	forced, err := lifecycle.Serve(ctx, server, tracker, 15*time.Second, func() error { return server.Serve(listener) })
	cancel()
	localServers.Wait()
	select {
	case localErr := <-localErrors:
		err = errors.Join(err, localErr)
	default:
	}
	if forced {
		log.Printf("writer: tempo di arresto scaduto; connessioni attive interrotte")
	}
	return err
}

func resolveUID(name string) (int, error) {
	if name == "" {
		return os.Geteuid(), nil
	}
	if uid, err := strconv.Atoi(name); err == nil && uid >= 0 {
		return uid, nil
	}
	account, err := user.Lookup(name)
	if err != nil {
		return 0, fmt.Errorf("utente locale %s: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, fmt.Errorf("UID non valido per %s", name)
	}
	return uid, nil
}

func setSocketGroup(path, name string) error {
	if name == "" {
		return nil
	}
	gid, parseErr := strconv.Atoi(name)
	if parseErr != nil || gid < 0 {
		group, err := user.LookupGroup(name)
		if err != nil {
			return fmt.Errorf("gruppo locale %s: %w", name, err)
		}
		gid, err = strconv.Atoi(group.Gid)
		if err != nil {
			return fmt.Errorf("GID non valido per %s", name)
		}
	}
	return os.Chown(path, -1, gid)
}

func listenPrivileged(path, group string, allow func(localapi.Peer) bool) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("il percorso socket privilegiata esistente non e una socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0660); err == nil {
		err = setSocketGroup(path, group)
	}
	if err != nil {
		listener.Close()
		return nil, err
	}
	return localapi.CredentialListener{Listener: listener, Allow: allow}, nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

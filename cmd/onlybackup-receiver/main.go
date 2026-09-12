package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/gateway"
	"github.com/meiome/onlybackup/internal/lifecycle"
	"github.com/meiome/onlybackup/internal/model"
)

func run() error {
	return runArgs(os.Args[1:])
}

func runArgs(args []string) error {
	flags := flag.NewFlagSet("onlybackup-receiver", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8443", "indirizzo HTTPS")
	socket := flags.String("socket", "./var/onlybackup/writer.sock", "socket del writer")
	cert := flags.String("tls-cert", "", "certificato PEM")
	key := flags.String("tls-key", "", "chiave TLS PEM")
	timeout := flags.Duration("upload-timeout", 2*time.Hour, "durata massima richiesta")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("argomenti inattesi")
	}
	if *cert == "" || *key == "" {
		return errors.New("--tls-cert e --tls-key obbligatori; HTTP in chiaro non disponibile")
	}
	if *timeout < time.Second {
		return errors.New("timeout troppo breve")
	}
	g := gateway.New(*socket)
	defer g.Close()
	tracker := lifecycle.NewTracker()
	server := &http.Server{Addr: *listen, Handler: tracker.Handler(g), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: *timeout, WriteTimeout: *timeout + time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: model.MaxHeaderBytes}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("receiver HTTPS in ascolto su %s", *listen)
	forced, err := lifecycle.Serve(ctx, server, tracker, 15*time.Second, func() error { return server.ListenAndServeTLS(*cert, *key) })
	if forced {
		log.Printf("receiver: tempo di arresto scaduto; connessioni attive interrotte")
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}

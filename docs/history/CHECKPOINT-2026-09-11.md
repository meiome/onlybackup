# Checkpoint OnlyBackup — 11 settembre 2026

> **Documento storico.** Descrive la versione a tre processi raggiunta l'11-12
> settembre 2026 e non va usato per installare la versione corrente. Per stato,
> installazione e sicurezza correnti leggere [README](../../README.md),
> [INSTALL](../INSTALL.md) e [SECURITY](../SECURITY.md).

Aggiornamento del 12 settembre: il client richiede ora `encrypt_to` per la
cifratura oppure il consenso esplicito `plaintext`; l'assenza di entrambe le
scelte interrompe l'invio prima della connessione.

Questo file registra il punto stabile raggiunto al termine del consolidamento.
Per i dettagli tecnici e i comandi aggiornati leggere [SECURITY](../SECURITY.md),
[INSTALL](../INSTALL.md) e [OPERATIONS](../OPERATIONS.md).

## Stato raggiunto

- Catena protetta completa: HTTPS receiver → writer → vault autorevole.
- Receiver e writer separati dall'archivio tramite utenti e gruppi Linux.
- Deposito cifrato configurato tramite destinatario age; il chiaro richiede consenso esplicito.
- Cifratura age X25519 eseguita sul client, con alternativa in chiaro esplicita e senza fallback.
- Chiave privata di recupero separata dai servizi di deposito.
- Recupero locale verificato, atomico e senza sovrascrittura.
- Arresto ordinato durante upload e riconciliazione ripetibile dopo interruzioni.
- Ritenti idempotenti nel protocollo v2 e rifiuto dei symlink per il file token.
- Schema SQLite v3 con lettura dei cataloghi v1/v2 e migrazione transazionale.
- Unit systemd di esempio per receiver, writer e vault.
- Documentazione di protocollo pubblico, protocollo vault, sicurezza,
  installazione e prove di recupero.

## Verifiche superate

- Suite Go completa con race detector.
- Analisi statica `go vet`.
- Smoke test HTTPS della modalità locale compatibile.
- Test dei binari reali con deposito chiaro/cifrato, revoca, riavvio e copia a freddo.
- Test Docker con UID distinti: writer e receiver non possono leggere,
  modificare, rinominare, sostituire o cancellare archivio e catalogo.
- Test di certificato non attendibile, redirect senza fuga della credenziale,
  timeout, ritento idempotente, chiavi errate, symlink del token rifiutati,
  contenuto alterato/troncato e file non regolari.
- Dump, deposito, recupero e importazione reali su MariaDB 10.11.18,
  in chiaro e cifrato, con verifica di dati UTF-8, aggregati e vincoli.

## Limiti ancora aperti

- Collaudo delle unità systemd sulla macchina definitiva.
- Protezione WORM/Object Lock o copia offline contro root o vault compromessi.
- Prove di carico, guasti fisici del disco e monitoraggio operativo.
- Test applicativi ulteriori, compreso MySQL 8 se necessario.
- Streaming e ripresa parziale, retention, ricevute firmate,
  aggiornamenti firmati e rotazione delle credenziali con storico quote.
- Pubblicazione del repository e preparazione della pagina principale.

## Punto di ripartenza

La prossima attività prioritaria è predisporre un server Linux dedicato di prova,
installare le tre unità systemd e ripetere i controlli descritti in INSTALL.
Subito dopo va scelto il livello indipendente di conservazione: storage WORM,
Object Lock oppure copia offline verificata.

La directory non è un repository Git valido e questo checkpoint non corrisponde
a un commit. Non è stato installato o avviato alcun servizio permanente.

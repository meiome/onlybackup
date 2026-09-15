# OnlyBackup

OnlyBackup è un server di backup **deposit-only** per Linux: i client possono
inviare nuovi file, ma l'API pubblica non permette di leggerli, modificarli o
cancellarli.

Il trasferimento usa HTTPS con TLS 1.3. Normalmente il client cifra il contenuto
con [age](https://age-encryption.org/) prima dell'invio; il salvataggio in chiaro
deve essere scelto esplicitamente. Receiver e writer sono eseguiti con utenti
Linux separati, così il receiver pubblico non accede all'archivio.

```text
client → HTTPS receiver → socket Unix → writer ─┬─ file .backup
                                                └─ SQLite
```

> **Stato:** progetto in sviluppo. Suite, race detector, test di sistema e
> isolamento sono disponibili; le unità systemd devono ancora essere collaudate
> su un server dedicato reale.

## Compilazione e prova dai sorgenti

Servono Linux, Go 1.27 o successivo e GCC.

```bash
make build
make check
make smoke
```

`make smoke` avvia un ambiente HTTPS temporaneo, invia e recupera un backup,
verifica integrità e API vietate, quindi rimuove i dati di prova.

## Release verificabili

Le release per Linux AMD64 sono compilate su Ubuntu 22.04 da GitHub Actions solo
per tag nel formato `vX.Y.Z`, dopo suite, analisi statica e race detector. Sono
destinate a sistemi Linux basati su glibc compatibili con Ubuntu 22.04; non sono
compatibili con Alpine Linux/musl. Ogni release contiene l'archivio,
`SHA256SUMS` e un'attestazione della provenienza della build.

Dopo aver scaricato i due file della release, verificare prima di estrarre o
eseguire:

```bash
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSIONE_linux_amd64.tar.gz \
  --repo meiome/onlybackup
```

Quindi estrarre l'archivio ed entrare nella directory:

```bash
tar -xzf onlybackup_VERSIONE_linux_amd64.tar.gz
cd onlybackup_VERSIONE_linux_amd64
```

Il checksum rileva modifiche accidentali; l'attestazione collega l'archivio al
workflow e al commit che lo hanno prodotto. Non costituisce una garanzia che il
programma sia privo di vulnerabilità.

## Installazione e uso

Sia dall'archivio verificato sia dalla directory dei sorgenti, seguire
[docs/INSTALL.md](docs/INSTALL.md) per installare e configurare i due servizi.
La guida comprende la creazione della credenziale di deposito e del destinatario
pubblico age. Sul client, il file di configurazione è:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send.key",
  "encrypt_to": "age1..."
}
```

```bash
onlybackup send --config client.json \
  --description "Backup gestionale" backup.sql
```

Il comando restituisce una ricevuta JSON soltanto dopo il salvataggio verificato.
Per conservare volutamente il file leggibile sul server usare `--plaintext`.
OnlyBackup riceve file già preparati; per MySQL è disponibile
[scripts/backup-mysql.sh](scripts/backup-mysql.sh).

## Documentazione

- [Installazione protetta](docs/INSTALL.md)
- [Modello di sicurezza](docs/SECURITY.md)
- [Protocollo pubblico](docs/PROTOCOL.md)
- [Prove di ripristino](docs/RESTORE-TEST.md)

Distribuito con licenza [GNU AGPL-3.0](LICENSE).

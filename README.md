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
> isolamento sono disponibili; le unità systemd della release `v0.1.0` sono
> state collaudate su un server Debian 13 AMD64 dedicato.

## Compilazione e prova dai sorgenti

Servono Linux, Go 1.27 o successivo e GCC.

```bash
make build
make check
make smoke
```

`make smoke` avvia un ambiente HTTPS temporaneo, invia e recupera un backup,
verifica integrità e API vietate, quindi rimuove i dati di prova.

Per produrre localmente i due client Windows AMD64 in `bin/windows-amd64`:

```bash
make windows-client
```

## Release verificabili

Le release sono create da GitHub Actions soltanto per tag nel formato `vX.Y.Z`,
dopo suite, analisi statica e race detector. Il pacchetto server Linux AMD64 è
compilato su Ubuntu 22.04 ed è destinato a sistemi glibc compatibili; non è
compatibile con Alpine Linux/musl. Il pacchetto client Windows AMD64 contiene
gli eseguibili nativi `onlybackup.exe` e `onlybackup-recover.exe`, provati anche
su un runner Windows Server 2022. Ogni archivio ha un checksum in `SHA256SUMS` e
un'attestazione della provenienza della build.

La release `v0.1.0` precede il supporto Windows e contiene soltanto il pacchetto
Linux. Il pacchetto ZIP sarà disponibile dalla prima release successiva che
include queste modifiche.

Dopo aver scaricato i due file della release, verificare prima di estrarre o
eseguire:

```bash
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSIONE_linux_amd64.tar.gz \
  --repo meiome/onlybackup
```

Per Windows verificare nello stesso modo
`onlybackup_VERSIONE_windows_amd64.zip`; i comandi PowerShell completi sono nella
[guida del client Windows](docs/CLIENT-WINDOWS.md).

Quindi estrarre l'archivio ed entrare nella directory:

```bash
tar -xzf onlybackup_VERSIONE_linux_amd64.tar.gz
cd onlybackup_VERSIONE_linux_amd64
```

Il checksum rileva modifiche accidentali; l'attestazione collega l'archivio al
workflow e al commit che lo hanno prodotto. Non costituisce una garanzia che il
programma sia privo di vulnerabilità.

## Installazione e uso

Per il server seguire [docs/INSTALL-DEBIAN.md](docs/INSTALL-DEBIAN.md) su Debian
13 oppure [docs/INSTALL.md](docs/INSTALL.md) per migrazioni e altri sistemi
Linux. Per i PC che inviano backup sono disponibili guide distinte per
[Debian](docs/CLIENT-DEBIAN.md) e [Windows nativo](docs/CLIENT-WINDOWS.md), con
automazione Bash e PowerShell. Sul client, il file di configurazione è:

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

- [Installazione passo passo su Debian 13](docs/INSTALL-DEBIAN.md)
- [Client Debian](docs/CLIENT-DEBIAN.md)
- [Client Windows nativo](docs/CLIENT-WINDOWS.md)
- [Installazione protetta](docs/INSTALL.md)
- [Modello di sicurezza](docs/SECURITY.md)
- [Protocollo pubblico](docs/PROTOCOL.md)
- [Prove di ripristino](docs/RESTORE-TEST.md)

Distribuito con licenza [GNU AGPL-3.0](LICENSE).

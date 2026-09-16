# Client OnlyBackup su Debian

Questa guida installa il client nativo su Debian AMD64, prepara cifratura e
credenziale e automatizza l'invio di file già prodotti. Per il server vedere
[INSTALL-DEBIAN.md](INSTALL-DEBIAN.md).

## Cosa serve

Il client usa tre elementi distinti:

- `onlybackup`: cifra, calcola il digest, autentica la richiesta e invia il file;
- una credenziale `send.key`: autorizza il deposito, ma da sola non può inviare;
- un recipient age pubblico: indica con quale identità privata cifrare.

`age-identity.txt` è la chiave privata di recupero. Non deve trovarsi sul server
di deposito e non va confusa con `send.key`.

Per backup reali creare sul server un profilo e una credenziale dedicati, con
nome riferibile al PC o al carico di lavoro. Non riutilizzare credenziali di
collaudo e non condividere la stessa chiave fra macchine indipendenti.

## 1. Verificare e installare il programma

Scaricare archivio Linux e `SHA256SUMS` dalla stessa release. Verificare
checksum e attestazione prima di estrarre:

```bash
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSIONE_linux_amd64.tar.gz \
  --repo meiome/onlybackup
tar -xzf onlybackup_VERSIONE_linux_amd64.tar.gz
```

Installare per il solo utente corrente:

```bash
install -d -m 0700 "$HOME/.local/bin"
install -m 0755 \
  onlybackup_VERSIONE_linux_amd64/bin/onlybackup \
  onlybackup_VERSIONE_linux_amd64/bin/onlybackup-recover \
  "$HOME/.local/bin/"
```

Verificare che `$HOME/.local/bin` sia nel `PATH`, quindi:

```bash
onlybackup --help
onlybackup-recover --help
```

## 2. Preparare la directory privata

```bash
install -d -m 0700 "$HOME/.config/onlybackup"
install -d -m 0700 "$HOME/.local/state/onlybackup/receipts"
```

Copiare nella directory di configurazione:

- la nuova credenziale di produzione come `send-production.key`;
- il certificato pubblico del server come `server.crt`, se usa una CA privata.

```bash
install -m 0600 send-production.key \
  "$HOME/.config/onlybackup/send-production.key"
install -m 0644 server.crt \
  "$HOME/.config/onlybackup/server.crt"
```

Dopo aver verificato la copia, eliminare l'esemplare consegnabile della
credenziale dal server. Nel catalogo server rimane soltanto il suo hash.

## 3. Creare o importare l'identità age

Se non esiste ancora un'identità di recupero:

```bash
onlybackup-recover keygen \
  --out "$HOME/.config/onlybackup/age-identity.txt" \
  > "$HOME/.config/onlybackup/age-public.json"
chmod 0600 "$HOME/.config/onlybackup/age-identity.txt"
```

Conservare una seconda copia protetta di `age-identity.txt`. Se viene persa, i
backup cifrati non sono recuperabili. Se esiste già un'identità valida, non
rigenerarla: copiare nella configurazione il recipient pubblico corrispondente.

## 4. Configurare il client

Creare `$HOME/.config/onlybackup/client.json`, sostituendo URL e recipient:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send-production.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

I percorsi relativi vengono risolti rispetto alla directory di `client.json`.
Se il certificato è emesso da una CA già fidata dal sistema, `ca_file` può
essere omesso.

```bash
chmod 0600 "$HOME/.config/onlybackup/client.json"
```

## 5. Inviare un file

Il client accetta un file regolare già pronto. Non esegue dump, snapshot,
compressione, selezione di directory o pianificazione.

```bash
onlybackup send \
  --config "$HOME/.config/onlybackup/client.json" \
  --description "Backup contabilità giornaliero" \
  --receipt "$HOME/.local/state/onlybackup/receipts/receipt-manuale.json" \
  /srv/export/contabilita.sql
```

La ricevuta viene creata soltanto con un nome nuovo e dopo la conferma del
server. Il client non sovrascrive una ricevuta esistente.

## 6. Automatizzare con Bash e cron

La release contiene `scripts/backup-file.sh`. Installarlo:

```bash
install -m 0755 scripts/backup-file.sh "$HOME/.local/bin/onlybackup-file"
```

Uso:

```bash
onlybackup-file \
  /srv/export/contabilita.sql \
  "$HOME/.config/onlybackup/client.json" \
  "$HOME/.local/state/onlybackup/receipts" \
  "Backup contabilità giornaliero"
```

Lo script genera un nome univoco per la ricevuta e delega cifratura, digest,
TLS e invio al client. Un esempio `cron`, con percorsi assoluti, è:

```cron
0 2 * * * /usr/bin/flock -n /home/utente/.local/state/onlybackup/send.lock /home/utente/.local/bin/onlybackup-file /srv/export/contabilita.sql /home/utente/.config/onlybackup/client.json /home/utente/.local/state/onlybackup/receipts "Backup contabilità giornaliero" >>/home/utente/.local/state/onlybackup/client.log 2>&1
```

Il processo che produce il file deve completare prima dell'invio. Per database
usare un dump consistente o uno snapshot applicativo; per MySQL è disponibile
anche `scripts/backup-mysql.sh`.

## 7. Provare il recupero

L'API è deposit-only: il client non può scaricare backup dal server. Il test di
recupero richiede una copia indipendente del file `.backup`, la ricevuta e
l'identità age.

```bash
onlybackup-recover \
  --receipt receipt.json \
  --archive ID_BACKUP.backup \
  --identity-file "$HOME/.config/onlybackup/age-identity.txt" \
  --out file-recuperato
```

Confrontare poi il file recuperato con l'originale o verificarlo
applicativamente. La lettura diretta di una copia completa dello stato Linux
resta disponibile tramite `--state DIRECTORY --id ID`.

## Checklist

- credenziale distinta da quelle di test e con permessi 0600;
- identità age custodita anche fuori dal PC di invio;
- TLS verificato, senza opzioni che disabilitino i controlli;
- ricevute conservate e incluse nelle procedure di ripristino;
- automazione provata manualmente con lo stesso utente di `cron`;
- recupero da copia indipendente eseguito periodicamente.

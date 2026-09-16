# Installazione su Debian 13 AMD64

Questa guida descrive un'installazione nuova dei due servizi OnlyBackup su un
server Debian 13 dedicato. La procedura è stata collaudata su Debian 13
(`x86_64`, glibc 2.41, systemd 257) con la release `v0.1.0`.

Per altri sistemi Linux, per migrare un archivio esistente o per rimuovere una
vecchia installazione con `onlybackup-vault`, vedere [INSTALL.md](INSTALL.md).

## Prima di iniziare

Servono:

- accesso SSH con un utente amministrativo autorizzato a usare `sudo`;
- Debian 13 AMD64, spazio sufficiente per archivio e temporanei;
- un certificato TLS valido per il nome DNS o l'indirizzo usato dai client;
- un PC fidato sul quale custodire credenziale di deposito e identità age;
- l'archivio della release e `SHA256SUMS` scaricati dalla stessa release.

Il server espone TCP 8443. Non occorre installare la chiave privata age sul
server: senza quella chiave il deposito non può decifrare i backup.

I comandi seguenti presuppongono di avere estratto l'archivio e di trovarsi
nella directory `onlybackup_VERSIONE_linux_amd64`.

## 1. Verificare la release sul PC fidato

Scaricare dalla pagina GitHub della stessa versione:

- `onlybackup_VERSIONE_linux_amd64.tar.gz`;
- `SHA256SUMS`.

Usare una GitHub CLI recente e autenticata. Prima di procedere verificare che
il comando `gh attestation` sia disponibile; le vecchie versioni Debian di `gh`
potrebbero non includerlo.

```bash
gh auth status
gh attestation --help >/dev/null
sha256sum --check SHA256SUMS
gh attestation verify onlybackup_VERSIONE_linux_amd64.tar.gz \
  --repo meiome/onlybackup
```

Non installare l'archivio se checksum o attestazione falliscono. Estrarlo solo
dopo entrambe le verifiche:

```bash
tar -xzf onlybackup_VERSIONE_linux_amd64.tar.gz
cd onlybackup_VERSIONE_linux_amd64
file bin/*
```

## 2. Controllare il server Debian

Prima di modificare la macchina verificare identità, architettura, sistema,
spazio e installazioni OnlyBackup eventualmente presenti:

```bash
hostnamectl
uname -m
getconf GNU_LIBC_VERSION
df -h /var/lib /var/tmp
systemctl list-unit-files 'onlybackup-*'
getent passwd onlybackup-writer onlybackup-receiver
getent group onlybackup-ingest onlybackup-receiver
```

Il risultato atteso per questa guida è Debian 13 e `x86_64`, senza unità
OnlyBackup già installate. Se esistono servizi o dati precedenti, non eseguire
la sezione di inizializzazione: seguire invece la migrazione o la
reinstallazione controllata in [INSTALL.md](INSTALL.md).

Installare soltanto gli strumenti di sistema necessari:

```bash
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates file openssl
```

## 3. Creare utenti e gruppi isolati

```bash
sudo groupadd --system onlybackup-ingest
sudo groupadd --system onlybackup-receiver

sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  --gid onlybackup-ingest \
  onlybackup-writer

sudo useradd \
  --system \
  --no-create-home \
  --shell /usr/sbin/nologin \
  --gid onlybackup-ingest \
  --groups onlybackup-receiver \
  onlybackup-receiver
```

Verificare prima di continuare:

```bash
id onlybackup-writer
id onlybackup-receiver
```

Il writer deve appartenere soltanto a `onlybackup-ingest`. Il receiver deve
avere `onlybackup-ingest` come gruppo primario e `onlybackup-receiver` come
gruppo supplementare.

## 4. Installare programmi e unità systemd

Eseguire dalla radice dell'archivio estratto:

```bash
sudo install -m 0755 \
  bin/onlybackup \
  bin/onlybackup-admin \
  bin/onlybackup-inspect \
  bin/onlybackup-recover \
  bin/onlybackup-receiver \
  bin/onlybackup-writer \
  /usr/local/bin/

sudo install -m 0644 \
  deploy/onlybackup-writer.service \
  deploy/onlybackup-receiver.service \
  /etc/systemd/system/

sudo systemd-analyze verify \
  /etc/systemd/system/onlybackup-writer.service \
  /etc/systemd/system/onlybackup-receiver.service

sudo systemctl daemon-reload
```

`systemd-analyze verify` non deve produrre errori.

## 5. Inizializzare lo stato

Questi comandi sono corretti soltanto per un'installazione nuova. Non eseguire
`init` sopra un archivio che deve essere conservato.

```bash
sudo install -d \
  -o onlybackup-writer \
  -g onlybackup-ingest \
  -m 0700 \
  /var/lib/onlybackup

sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup init
```

Controllare proprietà e permessi:

```bash
sudo stat -c '%A %a %U:%G %n' \
  /var/lib/onlybackup \
  /var/lib/onlybackup/incoming \
  /var/lib/onlybackup/backups \
  /var/lib/onlybackup/metadata.db
```

Le tre directory devono essere `0700`; il database deve essere `0600`. Tutto
deve appartenere a `onlybackup-writer:onlybackup-ingest`.

## 6. Installare il certificato TLS

Installare certificato e chiave già ottenuti dalla propria CA:

```bash
sudo install -d -o root -g onlybackup-receiver -m 0750 /etc/onlybackup
sudo install -o root -g onlybackup-receiver -m 0640 server.crt /etc/onlybackup/server.crt
sudo install -o root -g onlybackup-receiver -m 0640 server.key /etc/onlybackup/server.key
```

Controllare date e Subject Alternative Name. Il SAN deve contenere esattamente
il DNS o l'IP presente nell'URL client:

```bash
sudo openssl x509 \
  -in /etc/onlybackup/server.crt \
  -noout -subject -issuer -dates -ext subjectAltName

sudo openssl x509 \
  -in /etc/onlybackup/server.crt \
  -checkend 0 -noout

sudo -u onlybackup-receiver test \
  -r /etc/onlybackup/server.crt \
  -a -r /etc/onlybackup/server.key
```

Se si usa un certificato autofirmato, copiarne sul client soltanto la parte
pubblica `server.crt` e configurarla come `ca_file`. Non copiare `server.key`.

## 7. Creare profilo e credenziale di deposito

Elencare i profili predefiniti o crearne uno dedicato. Questo esempio ammette
200 GiB totali, 100 GiB per singolo backup, 24 invii ogni 24 ore e due invii
simultanei:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup profiles set \
  --name BACKUP \
  --total 200GiB \
  --max-backup 100GiB \
  --daily 24 \
  --concurrent 2
```

Creare una credenziale senza stamparla nel terminale:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys create \
  --name client-principale \
  --profile BACKUP \
  --out /tmp/onlybackup-send.key

sudo install -o "$(id -un)" -g "$(id -gn)" -m 0600 \
  /tmp/onlybackup-send.key \
  ./onlybackup-send.key

sudo rm -f -- /tmp/onlybackup-send.key
```

Trasferire `onlybackup-send.key` dalla directory corrente al PC fidato tramite
SSH, verificarne i permessi `0600` e poi eliminare la copia consegnabile dal
server. Nel catalogo rimane soltanto l'hash della credenziale.

## 8. Avviare e verificare i servizi

```bash
sudo systemctl enable --now \
  onlybackup-writer.service \
  onlybackup-receiver.service

sudo systemctl status \
  onlybackup-writer.service \
  onlybackup-receiver.service \
  --no-pager

sudo ss -ltnp 'sport = :8443'
sudo journalctl \
  -u onlybackup-writer.service \
  -u onlybackup-receiver.service \
  --since '-10 minutes' \
  --no-pager
```

Verificare anche che il receiver non possa attraversare o leggere lo stato:

```bash
sudo -u onlybackup-receiver test ! -x /var/lib/onlybackup
sudo -u onlybackup-receiver test ! -r /var/lib/onlybackup/metadata.db
```

Aprire TCP 8443 nel firewall solo per le reti o gli indirizzi che devono
depositare backup. La configurazione del firewall dipende dall'ambiente e non è
modificata da OnlyBackup.

## 9. Preparare il client

Le procedure complete, compresi automazione e recupero, sono separate per
[client Debian](CLIENT-DEBIAN.md) e [client Windows nativo](CLIENT-WINDOWS.md).

Sul PC fidato installare almeno `onlybackup` e `onlybackup-recover` dalla stessa
release. Generare l'identità age una sola volta:

```bash
onlybackup-recover keygen --out age-identity.txt > age-public.json
chmod 0600 age-identity.txt onlybackup-send.key
```

Custodire `age-identity.txt` separatamente dal server di deposito. Creare
`client.json` usando il recipient pubblico contenuto in `age-public.json`:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "onlybackup-send.key",
  "ca_file": "server.crt",
  "encrypt_to": "age1..."
}
```

Inviare un file piccolo e conservare la ricevuta:

```bash
onlybackup send \
  --config client.json \
  --description "Primo collaudo" \
  file-di-prova.bin > receipt.json
```

Sul server verificare che ID, dimensione e SHA-256 coincidano con la ricevuta e
che il file sia `0400`:

```bash
sudo -u onlybackup-writer \
  /usr/local/bin/onlybackup-admin --state /var/lib/onlybackup \
  backups status --id ID_DELLA_RICEVUTA

sudo stat -c '%A %a %U:%G %s %n' \
  /var/lib/onlybackup/backups/ID_DELLA_RICEVUTA.backup

sudo sha256sum \
  /var/lib/onlybackup/backups/ID_DELLA_RICEVUTA.backup
```

Completare il collaudo recuperando da una copia a freddo dell'intera directory
`/var/lib/onlybackup` e confrontando il file decifrato con l'originale. La
procedura dettagliata è in [RESTORE-TEST.md](RESTORE-TEST.md).

## Aggiornare una futura release

Prima dell'aggiornamento verificare la nuova release come al punto 1 e creare
una copia a freddo completa dello stato. Poi:

1. arrestare receiver e writer;
2. installare insieme tutti i sei binari e le due unità nuove;
3. eseguire `systemd-analyze verify` e `systemctl daemon-reload`;
4. riavviare writer e receiver;
5. verificare unità, porta, journal, nuovo deposito e recupero.

Non rieseguire `init`, non cancellare lo stato e non riaprire un catalogo
aggiornato con binari precedenti. Per una reinstallazione pulita o una vecchia
architettura con vault seguire [INSTALL.md](INSTALL.md).

## Criteri di completamento

L'installazione è pronta soltanto quando:

- writer e receiver sono attivi e abilitati;
- la porta 8443 è in ascolto e TLS verifica il nome usato dal client;
- il receiver non può leggere archivio e catalogo;
- la copia consegnabile della credenziale non è più sul server;
- upload cifrato, ricevuta, file, catalogo e SHA-256 sono coerenti;
- il recupero dalla copia a freddo produce un file identico all'originale;
- i journal non contengono errori rilevanti.

# Installazione su Linux dedicato

Le unità systemd fornite eseguono due processi con utenti distinti:

```text
client → receiver HTTPS → writer su socket Unix → archivio e catalogo
```

Il collaudo Docker prova i permessi Unix reali; l'avvio systemd sul server di
destinazione resta da verificare. I permessi proteggono l'archivio dal receiver,
ma non implementano immutabilità WORM contro root o contro il writer, che deve
poter creare file e aggiornare il catalogo. Vedere [SECURITY](SECURITY.md).

## Utenti e percorsi

| Componente | Utente | Gruppo primario | Gruppi supplementari |
|---|---|---|---|
| Writer | onlybackup-writer | onlybackup-ingest | nessuno |
| Receiver | onlybackup-receiver | onlybackup-ingest | onlybackup-receiver (TLS) |

Il writer possiede `/var/lib/onlybackup` con modo 0700 e accede a `incoming`,
`backups` e `metadata.db`. Il receiver può aprire soltanto la socket del writer.

- `/run/onlybackup`: writer:onlybackup-ingest, 0750; socket 0660.
- `/etc/onlybackup`: root:onlybackup-receiver, 0750; chiave TLS 0640.
- chiave privata age: custodita separatamente, mai necessaria ai servizi.

La directory della socket e il relativo lock non devono essere scrivibili dal
receiver. Non concedere al receiver ACL o gruppi che permettano di attraversare
la directory dell'archivio.

## Nuova installazione

Eseguire dalla radice dell'archivio binario estratto, oppure dai sorgenti dopo
`make build`:

```bash
sudo groupadd --system onlybackup-ingest
sudo groupadd --system onlybackup-receiver
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest onlybackup-writer
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest --groups onlybackup-receiver onlybackup-receiver
sudo install -m 0755 bin/onlybackup bin/onlybackup-admin bin/onlybackup-inspect bin/onlybackup-recover bin/onlybackup-receiver bin/onlybackup-writer /usr/local/bin/
sudo install -d -o onlybackup-writer -g onlybackup-ingest -m 0700 /var/lib/onlybackup
sudo -u onlybackup-writer /usr/local/bin/onlybackup-admin --state /var/lib/onlybackup init
sudo install -d -o root -g onlybackup-receiver -m 0750 /etc/onlybackup
```

Creare una credenziale di deposito, per esempio con il profilo `M`:

```bash
sudo -u onlybackup-writer /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys create \
  --name client-principale --profile M \
  --out /var/lib/onlybackup/send-client-principale.key
```

Consegnare il file `.key` al client tramite un canale sicuro e conservarlo con
permessi 0600. Dopo averne verificato la copia, eliminare l'esemplare in chiaro
dal server: nel catalogo resta soltanto il suo hash. Non lasciarlo accessibile al
receiver.

Sul client installare `onlybackup` e `onlybackup-recover`, quindi creare e
custodire separatamente l'identità age:

```bash
onlybackup-recover keygen --out age-identity.txt > age-public.json
chmod 0600 age-identity.txt
```

Senza `age-identity.txt` i backup cifrati non sono recuperabili. Il valore
`recipient` di `age-public.json` va copiato come `encrypt_to` nella configurazione
client; la chiave privata non va installata sul server di deposito.

Installare certificato e chiave TLS come `/etc/onlybackup/server.crt` e
`/etc/onlybackup/server.key`, proprietà root:onlybackup-receiver, con chiave 0640.
Poi installare e avviare le unità:

```bash
sudo install -m 0644 deploy/onlybackup-writer.service deploy/onlybackup-receiver.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now onlybackup-writer.service onlybackup-receiver.service
```

Systemd crea `/run/onlybackup`. Il writer ha `PrivateNetwork=yes` e accede in
scrittura allo stato; il receiver espone HTTPS sulla porta 8443, accede alla
socket Unix e ha `/var/lib/onlybackup` dichiarata inaccessibile. Predisporre
firewall e rinnovo del certificato.

Sul client creare `client.json`, tenendo `send.key` nella stessa directory:

```json
{
  "url": "https://backup.example.com:8443",
  "key_file": "send.key",
  "encrypt_to": "age1..."
}
```

Eseguire un primo deposito e conservare la ricevuta JSON:

```bash
onlybackup send --config client.json \
  --description "Primo backup di prova" backup.sql > receipt.json
```

## Reinstallazione pulita di un ambiente di prova

Usare questa procedura soltanto quando lo stato esistente non contiene backup,
profili o credenziali da conservare. I comandi sono limitati ai servizi e ai
percorsi di OnlyBackup; non richiedono il riavvio della macchina.

Fermare le vecchie unità e disabilitare l'eventuale vault:

```bash
sudo systemctl disable --now onlybackup-receiver.service onlybackup-writer.service
sudo systemctl disable --now onlybackup-vault.service 2>/dev/null || true
```

Eliminare lo stato sacrificabile e i soli componenti non più distribuiti:

```bash
sudo rm -rf -- /var/lib/onlybackup
sudo rm -f -- /etc/systemd/system/onlybackup-vault.service
sudo rm -f -- /usr/local/bin/onlybackup-vault
sudo systemctl daemon-reload
```

Installare insieme i binari e le due unità della nuova versione seguendo la
sezione "Nuova installazione", quindi inizializzare `/var/lib/onlybackup` come
`onlybackup-writer:onlybackup-ingest`. Gli utenti e i gruppi correnti possono
essere riutilizzati; l'eventuale appartenenza del writer al vecchio gruppo vault
va rimossa:

```bash
sudo gpasswd --delete onlybackup-writer onlybackup-vault-ingest 2>/dev/null || true
```

Il certificato TLS in `/etc/onlybackup` può essere mantenuto se è ancora valido.
Creare invece un nuovo profilo e una nuova credenziale di deposito: le
credenziali precedenti non appartengono al catalogo appena inizializzato. Dopo
aver aggiornato il client, avviare e verificare i due servizi:

```bash
sudo systemctl enable --now onlybackup-writer.service onlybackup-receiver.service
sudo systemctl status onlybackup-writer.service onlybackup-receiver.service --no-pager
```

## Migrazione da una versione con vault

La rimozione del processo `onlybackup-vault` non cambia il formato dei backup né
il percorso dello stato. Pianificare la migrazione con una finestra di arresto:

1. arrestare receiver, writer e il vecchio vault;
2. eseguire e verificare una copia a freddo dell'intera directory di stato,
   inclusi eventuali `metadata.db-wal` e `metadata.db-shm`;
3. installare insieme tutti i nuovi binari e le due nuove unità;
4. assegnare ricorsivamente lo stato a `onlybackup-writer:onlybackup-ingest`,
   mantenendo directory 0700, database 0600 e backup 0400;
5. rimuovere ACL e gruppi che davano accesso ai vecchi componenti, disabilitare
   `onlybackup-vault.service`, quindi avviare writer e receiver;
6. verificare un replay idempotente, un nuovo deposito e un recupero dalla copia.

Non avviare vecchio vault e nuovo writer sullo stesso archivio. Il lock
`writer.lock` impedisce l'uso simultaneo ai binari compatibili, ma non sostituisce
la procedura di arresto e la verifica dei processi.

## Schema SQLite e archivio esistente

I nuovi binari leggono cataloghi v1, v2, v3 e v4 in sola lettura. La prima
apertura in scrittura aggiorna alla v4 con transazioni: aggiunge progressivamente
`content_format`, la chiave di idempotenza e lo storico dei tentativi necessario
alla finestra mobile di 24 ore. Le righe scadute possono essere eliminate alla
successiva ammissione della stessa chiave.
La migrazione v3-v4 importa una volta il solo tentativo ricostruibile da
`backups.started_at`; i ritenti più vecchi già sovrascritti non sono ricostruibili.

I file preesistenti non vengono modificati o ricifrati. Non riaprire uno schema
v4 con vecchi binari. Per tornare indietro serve la copia completa precedente
alla migrazione; non esiste una migrazione inversa automatica.

## Collaudo e copia di sicurezza

Verificare sul server dedicato:

- avvio dei due servizi e invio HTTPS con certificato verificato;
- receiver incapace di leggere, modificare, rinominare o eliminare archivio e catalogo;
- invii oltre quota, disco quasi pieno, arresti durante upload e riconciliazione;
- recupero in chiaro e cifrato da una copia indipendente.

Per una copia consistente a freddo arrestare receiver e writer, non eseguire
comandi amministrativi durante la copia, copiare l'intero stato e poi riavviare.
Il test `make system-test` esegue questo ciclo su archivi temporanei.

Il limite di arresto ordinato applicativo è 15 secondi, seguito da interruzione
delle connessioni e attesa degli handler. Systemd concede 25 secondi. Un blocco
I/O oltre tale tempo può portare a SIGKILL e richiedere riconciliazione al
riavvio. L'assenza della ricevuta non prova che il deposito sia assente: ripetere
la stessa operazione con la stessa chiave di idempotenza.

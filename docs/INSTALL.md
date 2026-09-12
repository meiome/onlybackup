# Installazione su Linux dedicato

Le unità systemd fornite usano la modalità protetta con tre utenti distinti.
Non sono state installate o abilitate sulla macchina di sviluppo. Il collaudo
Docker prova i permessi Unix reali; l'avvio systemd sul server di destinazione
resta da verificare. Non è implementata immutabilità WORM contro root o vault
compromessi: vedere [SECURITY](SECURITY.md).

## Utenti e percorsi

| Componente | Utente | Gruppo primario | Gruppi supplementari |
|---|---|---|---|
| Vault | onlybackup-vault | onlybackup-vault-ingest | nessuno |
| Writer | onlybackup-writer | onlybackup-ingest | onlybackup-vault-ingest |
| Receiver | onlybackup-receiver | onlybackup-ingest | onlybackup-receiver (TLS) |

Solo il vault possiede `/var/lib/onlybackup` (0700), `incoming`, `backups` e
`metadata.db`. Il writer non possiede un catalogo separato da cui il recupero
possa dipendere. Quote, chiavi e contenuti sono verificati dal vault.

- `/run/onlybackup-vault`: vault:onlybackup-vault-ingest, 0750; socket 0660.
- `/run/onlybackup`: writer:onlybackup-ingest, 0750; socket writer 0660.
- `/etc/onlybackup`: root:onlybackup-receiver, 0750; chiave TLS 0640.
- Chiave privata age: custodita separatamente, mai necessaria ai tre servizi.

I gruppi delle due socket devono essere distinti: il receiver non deve poter
raggiungere direttamente il vault. Directory di socket e lock non devono essere
scrivibili dai rispettivi client. Non condividere l'archivio tramite SMB/NFS o
altri accessi scrivibili concessi al writer.

## Nuova installazione

Eseguire i comandi dalla radice dell'archivio binario estratto, che contiene già
`bin/`, oppure dalla radice dei sorgenti dopo:

```bash
make build
```

Su una macchina dedicata nuova:

```bash
sudo groupadd --system onlybackup-ingest
sudo groupadd --system onlybackup-vault-ingest
sudo groupadd --system onlybackup-receiver
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-vault-ingest onlybackup-vault
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest --groups onlybackup-vault-ingest onlybackup-writer
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --gid onlybackup-ingest --groups onlybackup-receiver onlybackup-receiver
sudo install -m 0755 bin/onlybackup bin/onlybackup-admin bin/onlybackup-inspect bin/onlybackup-recover bin/onlybackup-receiver bin/onlybackup-writer bin/onlybackup-vault /usr/local/bin/
sudo install -d -o onlybackup-vault -g onlybackup-vault-ingest -m 0700 /var/lib/onlybackup
sudo -u onlybackup-vault /usr/local/bin/onlybackup-admin --state /var/lib/onlybackup init
sudo install -d -o root -g onlybackup-receiver -m 0750 /etc/onlybackup
```

L'inizializzazione crea i profili predefiniti `XS`, `S`, `M`, `L` e `XL`.
Creare una credenziale di deposito, per esempio con il profilo `M`:

```bash
sudo -u onlybackup-vault /usr/local/bin/onlybackup-admin \
  --state /var/lib/onlybackup keys create \
  --name client-principale --profile M \
  --out /var/lib/onlybackup/send-client-principale.key
```

Consegnare il file `.key` al client tramite un canale sicuro e conservarlo con
permessi 0600. Dopo averne verificato la copia, eliminare l'esemplare in chiaro
dal server: nel catalogo resta soltanto il suo hash. Non inviarlo per e-mail o
inserirlo in Git.

Sulla macchina client fidata, dalla radice dello stesso archivio verificato o dei
sorgenti compilati, installare il client e lo strumento di recupero:

```bash
sudo install -m 0755 bin/onlybackup bin/onlybackup-recover /usr/local/bin/
```

Generare quindi l'identità age di recupero:

```bash
onlybackup-recover keygen --out age-identity.txt > age-public.json
chmod 0600 age-identity.txt
```

Custodire e duplicare in modo sicuro `age-identity.txt`: senza questo file i
backup cifrati non sono recuperabili. `age-public.json` contiene il valore
`recipient`, che può essere copiato come `encrypt_to` nella configurazione del
client; la chiave privata non deve essere installata sui servizi di deposito.

Installare un certificato TLS valido per il nome usato dai client come
`/etc/onlybackup/server.crt`, e la relativa chiave come
`/etc/onlybackup/server.key`, proprietà root:onlybackup-receiver e permessi 0640.
La unità del receiver include esplicitamente il gruppo TLS supplementare.

```bash
sudo install -m 0644 deploy/onlybackup-vault.service deploy/onlybackup-writer.service deploy/onlybackup-receiver.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now onlybackup-vault.service onlybackup-writer.service onlybackup-receiver.service
```

Systemd crea le directory runtime. Il receiver ascolta sulla porta 8443: predisporre
firewall e rinnovo dei certificati. Writer e vault hanno `PrivateNetwork=yes` e
`RestrictAddressFamilies=AF_UNIX`; il receiver può raggiungere soltanto la socket
writer, con archivio e runtime vault inaccessibili. Il writer usa `--vault-socket`
e non apre lo stato locale; `--state` non gli assegna accesso in tale modalità.

Amministrare profili e credenziali localmente come vault o root. Una nuova chiave
di deposito può essere creata in un file nuovo nella directory dell'archivio e
poi consegnata in modo controllato al client, che la conserva 0600. Non lasciare
al receiver accesso ai file delle chiavi. Il comando di recupero si esegue in un
ambiente locale autorizzato: la chiave privata age si usa soltanto quando serve.

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

## Archivio esistente e schema SQLite

I nuovi binari leggono cataloghi v1, v2 e v3 in sola lettura. La prima apertura in
scrittura aggiorna alla v3 con una transazione: per la v1 aggiunge `content_format`,
lasciando i vecchi record in chiaro, e per v1/v2 aggiunge la chiave di idempotenza.
Aggiornare tutti i comandi prima di riaprire uno schema v3 con binari precedenti.

Per un archivio esistente procedere con servizi arrestati, copia a freddo
verificata dell'intero stato (anche eventuali `metadata.db-wal` e `metadata.db-shm`)
e prova dei nuovi binari sulla copia. Per il passaggio alla modalità protetta,
assegnare lo stato al nuovo utente vault e rimuovere accessi/ACL del vecchio writer;
controllare anche i genitori delle directory. Questa modifica dei permessi non
viene eseguita automaticamente dal programma. Non attivare contemporaneamente
vault e writer locale sullo stesso stato: il lock condiviso lo impedisce.

I file preesistenti non vengono ricifrati. Per un ritorno ai vecchi binari serve
lo stato v1 salvato prima dell'aggiornamento; non esiste una migrazione inversa
automatica. Una copia storica non contiene i depositi ricevuti dopo la copia:
conservarli separatamente e verificare il recupero prima di un eventuale ritorno.

## Collaudo e copia di sicurezza

Verificare sul server dedicato:

- avvio dei tre servizi e invio HTTPS con certificato verificato;
- writer e receiver incapaci di leggere, chmod, rinominare, sostituire o eliminare
  i contenuti e il catalogo; receiver incapace di aprire la socket vault;
- invii oltre quota e disco quasi pieno, arresti durante upload e riconciliazione;
- recupero in chiaro/cifrato e ripristino del catalogo da copie indipendenti.

Per una copia consistente a freddo arrestare receiver, writer e vault (e non
eseguire comandi amministrativi durante la copia), copiare l'intera directory
dello stato su un'altra destinazione e riavviare i servizi. Verificare il recupero
dalla copia; custodire separatamente le chiavi private age. Il test automatico
`make system-test` esegue questo ciclo su archivi temporanei.

Il limite di arresto ordinato applicativo è 15 secondi, seguito da interruzione
delle connessioni e attesa degli handler. Systemd concede 25 secondi: un blocco
I/O oltre tale tempo può portare a SIGKILL e richiedere riconciliazione al riavvio.
Nessuna ricevuta persa deve essere interpretata come prova di mancato deposito.
